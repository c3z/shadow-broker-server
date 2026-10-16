"""
shadow_client.py
Loads all configuration from a single SHADOW_BROKER_CONFIG env variable
(base64-encoded JSON). Provides publish, subscribe, and ping.
"""

import os
import sys
import time
import json
import base64
import logging
import threading
from dataclasses import dataclass, field

import requests
import websocket

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    stream=sys.stdout,
)


@dataclass
class ShadowBrokerConfig:
    """Stores broker configuration."""

    url: str
    auth_token: str
    secret_key: str = None


@dataclass
class ShadowClientPy:
    """A Python client for the ShadowBroker."""

    conf: ShadowBrokerConfig = None

    def __post_init__(self):
        self.conf = self._load_config()
        logging.info(f"ShadowClientPy initialized with URL={self.conf.url}")

    @staticmethod
    def _load_config() -> ShadowBrokerConfig:
        """
        Loads a single base64-encoded JSON from SHADOW_BROKER_CONFIG env.
        Example JSON:
        {
          "url": "https://my-broker.fly.dev",
          "auth_token": "MyAuthSecret",
          "secret_key": "12345678901234567890123456789012"  # optional, unused (encryption is server-side)
        }
        """
        b64 = os.environ.get("SHADOW_BROKER_CONFIG")
        if not b64:
            raise RuntimeError("Missing SHADOW_BROKER_CONFIG in environment")

        decoded = base64.b64decode(b64).decode("utf-8")
        try:
            parsed = json.loads(decoded)
        except json.JSONDecodeError as e:
            raise ValueError(f"Error decoding SHADOW_BROKER_CONFIG JSON: {e}")

        if not all(k in parsed for k in ("url", "auth_token")):
            raise ValueError("SHADOW_BROKER_CONFIG missing required keys")

        return ShadowBrokerConfig(
            url=parsed["url"],
            auth_token=parsed["auth_token"],
            secret_key=parsed.get("secret_key"),
        )

    def publish(self, queue: str, message: str, lane: str | None = None) -> str:
        """
        Publishes a message to the specified queue via HTTP POST.
        Returns the ID of the newly published message.
        lane="control" sets X-Broker-Lane (separate rate-limit budget on the
        broker); dispatch priority comes from the queue name ending in ".control".
        """
        endpoint = self.conf.url.rstrip("/") + "/publish"
        headers = {"Content-Type": "application/json", "X-Auth": self.conf.auth_token}
        if lane:
            headers["X-Broker-Lane"] = lane
        payload = {"queue": queue, "message": message}

        logging.info(f"Publishing to queue='{queue}' => {message!r}")
        resp = requests.post(endpoint, headers=headers, json=payload, timeout=10)
        resp.raise_for_status()

        data = resp.json()
        msg_id = data.get("id")
        logging.info(f"Published message ID={msg_id} for queue='{queue}'")
        return msg_id

    def subscribe(self, queue: str, on_message, on_open=None, on_close=None) -> None:
        """
        Subscribes to a queue via WebSocket in a background thread.
        `on_message` is a callback that handles incoming messages (plaintext).
        Optional `on_open` and `on_close` allow hooking into WS lifecycle events.
        """

        def _run_ws():
            ws_url = self._build_ws_url(queue)
            logging.info(f"Connecting WS for queue='{queue}' => {ws_url}")

            def _on_open(ws_conn):
                logging.info(f"WebSocket open for queue='{queue}'")
                # Send auth token as first message after connection
                logging.info("Sending auth token...")
                ws_conn.send(json.dumps({"token": self.conf.auth_token}))
                if on_open:
                    on_open(ws_conn)

            def _on_close(ws_conn, code, reason):
                logging.info(
                    f"WebSocket closed for queue='{queue}', code={code}, reason={reason}"
                )
                if on_close:
                    on_close(ws_conn, code, reason)

            def _on_message(ws_conn, raw_msg):
                try:
                    msg = json.loads(raw_msg)
                except json.JSONDecodeError:
                    logging.error("Failed to parse incoming WS message as JSON.")
                    return

                match msg.get("type"):
                    case "message":
                        on_message(msg["message"])
                    case "pong":
                        logging.info("Received PONG on WS.")
                    case _:
                        logging.debug(f"Unknown message type: {msg}")

            # Create WebSocket app without auth header
            ws_app = websocket.WebSocketApp(
                ws_url,
                on_open=_on_open,
                on_close=_on_close,
                on_message=_on_message,
            )

            # This call blocks until the WS is closed, so run it in a thread
            ws_app.run_forever()

        thread = threading.Thread(target=_run_ws, daemon=True)
        thread.start()

    def ping(self) -> None:
        """
        Sends a ping through a temporary ephemeral WS. We open a new
        WebSocket to queue='_ping' and send {"type":"ping"}. We'll log any PONG,
        then close after 2s. Useful for checking broker reachability.
        """

        def _on_open(ws_conn):
            logging.info("Ping WS opened, sending auth token...")
            # Send auth token first
            ws_conn.send(json.dumps({"token": self.conf.auth_token}))

            # Then send ping
            logging.info("Sending 'ping' message.")
            ws_conn.send(json.dumps({"type": "ping"}))

            # close after short delay
            def _closer():
                time.sleep(2)
                logging.info("Closing ping WS.")
                ws_conn.close()

            threading.Thread(target=_closer, daemon=True).start()

        def _on_message(ws_conn, raw_msg):
            try:
                msg = json.loads(raw_msg)
                if msg.get("type") == "pong":
                    logging.info("Received PONG from ephemeral WS.")
            except json.JSONDecodeError:
                logging.warning("Could not parse ephemeral WS message as JSON.")

        ws_url = self._build_ws_url("_ping")
        logging.info(f"Pinging via ephemeral WS => {ws_url}")
        # Create websocket without auth header
        ws_app = websocket.WebSocketApp(
            ws_url, on_open=_on_open, on_message=_on_message
        )

        def _temp_ws():
            ws_app.run_forever()

        threading.Thread(target=_temp_ws, daemon=True).start()

    def _build_ws_url(self, queue: str) -> str:
        """Helper to transform http(s) to ws(s) and append queue/auth params."""
        # If the config URL is http:// or https://, replace it with ws:// / wss://
        if self.conf.url.startswith("https://"):
            prefix = "wss://"
            suffix = self.conf.url.removeprefix("https://")
        elif self.conf.url.startswith("http://"):
            prefix = "ws://"
            suffix = self.conf.url.removeprefix("http://")
        else:
            # fallback if somehow not recognized
            prefix = "ws://"
            suffix = self.conf.url

        base = prefix + suffix.rstrip("/")
        return f"{base}/ws?queue={queue}"

    def pull(self, queue: str, since: int = 0, limit: int = 100) -> dict:
        """
        Pulls messages from the specified queue with pagination.

        Args:
            queue: The name of the queue to pull from
            since: Only return messages with ID > since (default: 0)
            limit: Maximum number of messages to return (default: 100, max: 1000)

        Returns:
            A dictionary containing:
                - messages: List of message objects with id, message, and created_at
                - last_id: The ID of the last message in the result set
        """
        if not queue or not isinstance(queue, str):
            raise ValueError("Queue name must be a non-empty string")

        if not isinstance(since, int) or since < 0:
            raise ValueError("since must be a non-negative integer")

        if not isinstance(limit, int) or limit <= 0 or limit > 1000:
            raise ValueError("limit must be a positive integer up to 1000")

        endpoint = self.conf.url.rstrip("/") + "/pull"
        headers = {"X-Auth": self.conf.auth_token}
        params = {"queue": queue, "since": str(since), "limit": str(limit)}

        logging.info(f"Pulling from queue='{queue}' (since={since}, limit={limit})")
        resp = requests.get(endpoint, headers=headers, params=params, timeout=10)
        resp.raise_for_status()

        result = resp.json()
        if (
            not isinstance(result, dict)
            or "messages" not in result
            or "last_id" not in result
        ):
            raise ValueError("Invalid response from server")

        logging.info(
            f"Pulled {len(result['messages'])} messages from queue='{queue}', last_id={result['last_id']}"
        )
        return result


# =============== Example usage ===============
if __name__ == "__main__":

    def handle_msg(text: str) -> None:
        logging.info(f"Received message on 'demo' => {text!r}")

    try:
        client = ShadowClientPy()

        # Subscribe
        client.subscribe("demo", on_message=handle_msg)

        # Publish
        msg_id = client.publish("demo", "Hello from future Python 2025!")
        logging.info(f"Publish returned ID={msg_id}")

        # Pull
        try:
            logging.info("Pulling messages from 'demo' queue")
            pull_result = client.pull("demo", since=0, limit=10)
            logging.info(
                f"Pulled {len(pull_result['messages'])} messages, last_id={pull_result['last_id']}"
            )

            # Print pulled messages
            if pull_result["messages"]:
                for msg in pull_result["messages"]:
                    logging.info(f"Message ID={msg['id']}, Content: {msg['message']!r}")
        except Exception as e:
            logging.error(f"Pull error: {e}")

        # Ping
        logging.info("Sending ping to ephemeral WS.")
        client.ping()

        # Wait a few seconds for messages to arrive
        time.sleep(5)
        logging.info("Done with main example. Exiting.")

    except Exception as e:
        logging.error(f"Main: an error occurred => {e}")
        sys.exit(1)
