#!/usr/bin/env python3
"""
Hardcore stress test for the ShadowBroker, Python version.
Similar to testhard.js but implemented in Python.
"""

import json
import time
import random
import threading
import statistics
from typing import Dict, List, Set, Any
from dataclasses import dataclass, field

from shadow_client import ShadowClientPy

# Configuration
TEST_DURATION_SEC = 60  # 1 minute test
CHANNELS = ["stress_small", "stress_medium", "stress_large", "stress_order"]
PUBLISHERS_PER_CHANNEL = 3
SUBSCRIBERS_PER_CHANNEL = 5
MESSAGE_RATES = {
    "stress_small": 100,  # msgs per second target (very high frequency)
    "stress_medium": 50,  # msgs per second target
    "stress_large": 10,  # msgs per second target (large messages)
    "stress_order": 200,  # msgs per second target (to test ordering)
}


# Statistics tracking
@dataclass
class Statistics:
    """Track test statistics"""

    published: Dict[str, int] = field(default_factory=lambda: {"total": 0})
    received: Dict[str, int] = field(default_factory=lambda: {"total": 0})
    missed: Dict[str, int] = field(default_factory=lambda: {"total": 0})
    latencies: List[float] = field(default_factory=list)
    errors: int = 0
    out_of_order: int = 0
    latency_lock: threading.Lock = field(default_factory=threading.Lock)
    stats_lock: threading.Lock = field(default_factory=threading.Lock)


stats = Statistics()

# Initialize per-channel stats
for channel in CHANNELS:
    stats.published[channel] = 0
    stats.received[channel] = 0

# Flag to signal test termination
test_running = True


def setup_subscribers(clients: List[ShadowClientPy]) -> None:
    """Setup subscriber clients listening on test channels"""
    client_index = 0

    for channel in CHANNELS:
        # Track received messages for order verification
        received_sequences: Dict[str, Dict[str, int]] = {}

        for i in range(SUBSCRIBERS_PER_CHANNEL):
            client = clients[client_index]
            client_index += 1
            subscriber_id = f"{channel}_sub_{i}"

            # Keep track of received message sequences for this subscriber
            received_sequences[subscriber_id] = {}

            def on_message(
                message_text: str,
                sub_id=subscriber_id,
                chan=channel,
                sequences=received_sequences[subscriber_id],
            ):
                try:
                    data = json.loads(message_text)

                    # Handle termination message
                    if data.get("type") == "termination":
                        print(f"{sub_id}: Received termination signal")
                        return

                    # Record message receipt
                    with stats.stats_lock:
                        stats.received["total"] += 1
                        stats.received[chan] = stats.received.get(chan, 0) + 1

                    # Calculate latency
                    latency = time.time() * 1000 - data.get("timestamp", 0)
                    with stats.latency_lock:
                        stats.latencies.append(latency)

                    # Check message ordering for the order test channel
                    if chan == "stress_order":
                        publisher_id = data.get("publisherId")
                        sequence = data.get("sequence")

                        if publisher_id not in sequences:
                            sequences[publisher_id] = sequence
                        else:
                            last_sequence = sequences[publisher_id]
                            if sequence != last_sequence + 1:
                                with stats.stats_lock:
                                    stats.out_of_order += 1
                                print(
                                    f"{sub_id}: Out of order message from {publisher_id}. "
                                    f"Expected {last_sequence + 1}, got {sequence}"
                                )
                            sequences[publisher_id] = sequence

                except Exception as e:
                    with stats.stats_lock:
                        stats.errors += 1
                    print(f"{sub_id} error parsing message: {e}")

            try:
                # Subscribe with a custom message handler for this subscriber
                client.subscribe(channel, on_message=on_message)
                print(f"{subscriber_id} started and listening to '{channel}'")
            except Exception as e:
                print(f"Error setting up subscriber {subscriber_id}: {e}")
                # Try once more
                try:
                    time.sleep(0.2)  # Wait a bit before retrying
                    client.subscribe(channel, on_message=on_message)
                    print(
                        f"{subscriber_id} started on retry and listening to '{channel}'"
                    )
                except Exception as e2:
                    print(
                        f"Failed to set up subscriber {subscriber_id} after retry: {e2}"
                    )


def start_publishing(
    client: ShadowClientPy, channel: str, publisher_index: int
) -> None:
    """Start publishing messages at the rate specified for the channel"""
    publisher_id = f"{channel}_pub_{publisher_index}"
    sequence = 0

    print(f"Publisher {publisher_id} started on channel {channel}")

    # Calculate delay based on target message rate
    target_delay_sec = 1.0 / MESSAGE_RATES.get(channel, 10)

    # Track timing to better control message rate
    last_publish_time = time.time()

    # Run until test ends
    global test_running
    while test_running:
        try:
            current_time = time.time()
            # Calculate the actual delay needed based on the last publish time
            # This helps maintain a steady rate even if message sending takes time
            elapsed = current_time - last_publish_time
            actual_delay = max(0.001, target_delay_sec - elapsed)

            timestamp = int(current_time * 1000)

            # Create different message sizes based on channel
            payload = None  # Initialize payload to avoid "possibly unbound" error
            if channel == "stress_small":
                payload = {
                    "type": "data",
                    "publisherId": publisher_id,
                    "sequence": sequence,
                    "timestamp": timestamp,
                    "value": random.random(),
                }
            elif channel == "stress_medium":
                payload = {
                    "type": "data",
                    "publisherId": publisher_id,
                    "sequence": sequence,
                    "timestamp": timestamp,
                    "value": random.random(),
                    "data": [random.random() for _ in range(100)],
                }
            elif channel == "stress_large":
                payload = {
                    "type": "data",
                    "publisherId": publisher_id,
                    "sequence": sequence,
                    "timestamp": timestamp,
                    "value": random.random(),
                    # Create a large nested object
                    "data": [
                        {
                            "id": i,
                            "values": [random.random() for _ in range(20)],
                            "text": "".join(
                                random.choice("abcdefghijklmnopqrstuvwxyz")
                                for _ in range(200)
                            ),
                        }
                        for i in range(500)
                    ],
                }
            elif channel == "stress_order":
                payload = {
                    "type": "data",
                    "publisherId": publisher_id,
                    "sequence": sequence,
                    "timestamp": timestamp,
                    "value": random.random(),
                }

            # Skip publishing if payload is None
            if payload is None:
                with stats.stats_lock:
                    stats.errors += 1
                print(f"{publisher_id} error: No payload created for channel {channel}")
                time.sleep(0.1)
                continue

            sequence += 1
            publish_start = time.time()
            client.publish(channel, json.dumps(payload))
            publish_time = time.time() - publish_start

            # Update statistics
            with stats.stats_lock:
                stats.published["total"] += 1
                stats.published[channel] = stats.published.get(channel, 0) + 1

            # Update last publish time
            last_publish_time = time.time()

            # Add jitter to make the test more realistic
            jitter = random.random() * target_delay_sec * 0.3
            delay = max(0.001, target_delay_sec - jitter)

            # For burst testing, occasionally send a batch of messages with minimal delay
            if random.random() < 0.05:  # 5% chance of burst
                burst_count = random.randint(5, 15)  # 5-15 messages in burst
                for i in range(burst_count):
                    if not test_running:
                        break

                    try:
                        burst_payload = {
                            "type": "burst",
                            "publisherId": publisher_id,
                            "sequence": sequence,
                            "timestamp": int(time.time() * 1000),
                            "burstId": i,
                            "burstTotal": burst_count,
                        }

                        sequence += 1
                        client.publish(channel, json.dumps(burst_payload))

                        with stats.stats_lock:
                            stats.published["total"] += 1
                            stats.published[channel] = (
                                stats.published.get(channel, 0) + 1
                            )

                        # Minimal delay between burst messages with small sleep to allow other threads to run
                        time.sleep(0.005)  # Slightly longer delay to ensure processing
                    except Exception as e:
                        with stats.stats_lock:
                            stats.errors += 1
                        print(f"{publisher_id} error in burst publishing: {e}")

                # Add a small delay after bursts to allow processing to catch up
                time.sleep(0.02)

            # Sleep for calculated delay
            time.sleep(actual_delay)

        except Exception as e:
            with stats.stats_lock:
                stats.errors += 1
            print(f"{publisher_id} error publishing: {e}")
            # Brief pause on error
            time.sleep(0.1)


def print_results() -> None:
    """Print test results and statistics"""
    # Calculate statistics
    total_published = stats.published["total"]
    total_received = stats.received["total"]
    expected_received = total_published * SUBSCRIBERS_PER_CHANNEL
    stats.missed["total"] = expected_received - total_received

    # Calculate latency statistics
    if stats.latencies:
        avg_latency = sum(stats.latencies) / len(stats.latencies)
        min_latency = min(stats.latencies)
        max_latency = max(stats.latencies)
        try:
            median_latency = statistics.median(stats.latencies)
            p95_latency = statistics.quantiles(stats.latencies, n=20)[
                18
            ]  # 95th percentile
        except:
            # Use float values instead of strings to avoid type errors
            median_latency = 0.0
            p95_latency = 0.0
    else:
        avg_latency = min_latency = max_latency = median_latency = p95_latency = 0

    # Per-channel statistics
    channel_stats = []
    for channel in CHANNELS:
        published = stats.published.get(channel, 0)
        received = stats.received.get(channel, 0)
        expected_channel_received = published * SUBSCRIBERS_PER_CHANNEL
        missed = expected_channel_received - received
        delivery_rate = (
            (received / expected_channel_received * 100)
            if expected_channel_received > 0
            else 0
        )

        channel_stats.append(
            {
                "channel": channel,
                "published": published,
                "received": received,
                "expected_received": expected_channel_received,
                "missed": missed,
                "delivery_rate": f"{delivery_rate:.2f}%",
            }
        )

    print("\n========== HARDCORE BROKER TEST RESULTS ==========")
    print(f"Test duration: {TEST_DURATION_SEC} seconds")
    print(f"Total messages published: {total_published}")
    print(f"Expected message deliveries: {expected_received}")
    print(f"Total messages received: {total_received}")
    print(f"Missed deliveries: {stats.missed['total']}")
    print(f"Delivery rate: {(total_received / expected_received * 100):.2f}%")
    print(f"Avg latency: {avg_latency:.2f}ms")
    print(f"Median latency: {median_latency}ms")
    print(f"95th percentile latency: {p95_latency}ms")
    print(f"Min latency: {min_latency:.2f}ms")
    print(f"Max latency: {max_latency:.2f}ms")
    print(f"Out of order messages: {stats.out_of_order}")
    print(f"Errors: {stats.errors}")

    print("\n--- Channel Statistics ---")
    for cs in channel_stats:
        print(
            f"{cs['channel']}: Published={cs['published']}, Received={cs['received']}, "
            f"Expected={cs['expected_received']}, Missed={cs['missed']}, "
            f"Delivery Rate={cs['delivery_rate']}"
        )

    print("================================================\n")

    # Overall test assessment
    delivery_rate = (
        (total_received / expected_received * 100) if expected_received > 0 else 0
    )
    if delivery_rate > 99.5 and stats.out_of_order == 0 and stats.errors == 0:
        print(
            "TEST RESULT: EXCELLENT - Broker demonstrated exceptional stability and performance"
        )
    elif delivery_rate > 98 and stats.out_of_order < 100 and stats.errors < 10:
        print("TEST RESULT: GOOD - Broker performed well with minimal issues")
    elif delivery_rate > 95:
        print(
            "TEST RESULT: ACCEPTABLE - Broker functioned but showed some stability issues"
        )
    else:
        print("TEST RESULT: POOR - Broker exhibited significant reliability problems")


def run_hardcore_test():
    """Main function to run the hardcore broker stress test"""
    print("Starting hardcore broker stress test...")

    # Create client instances for publishers and subscribers
    publisher_clients = []
    subscriber_clients = []

    # Create publisher clients
    for _ in range(len(CHANNELS) * PUBLISHERS_PER_CHANNEL):
        try:
            publisher_clients.append(ShadowClientPy())
        except Exception as e:
            print(f"Error creating publisher client: {e}")
            # Try once more
            time.sleep(0.5)
            publisher_clients.append(ShadowClientPy())

    # Create subscriber clients
    for _ in range(len(CHANNELS) * SUBSCRIBERS_PER_CHANNEL):
        try:
            subscriber_clients.append(ShadowClientPy())
        except Exception as e:
            print(f"Error creating subscriber client: {e}")
            # Try once more
            time.sleep(0.5)
            subscriber_clients.append(ShadowClientPy())

    # Allow connections to establish
    time.sleep(1.5)  # Slightly longer for connection establishment

    # Setup subscribers
    setup_subscribers(subscriber_clients)

    # Start publishing on all channels
    publisher_threads = []
    channel_index = 0

    for i, client in enumerate(publisher_clients):
        channel = CHANNELS[channel_index]
        thread = threading.Thread(
            target=start_publishing, args=(client, channel, i), daemon=True
        )
        thread.start()
        publisher_threads.append(thread)

        channel_index = (channel_index + 1) % len(CHANNELS)

    # Let test run for specified duration
    time.sleep(TEST_DURATION_SEC)

    # Signal test end and send termination messages
    global test_running
    test_running = False
    print("Test duration reached, sending termination signals...")

    # Send termination messages to each channel
    for channel in CHANNELS:
        try:
            for _ in range(
                3
            ):  # Send termination message multiple times to ensure delivery
                publisher_clients[0].publish(
                    channel,
                    json.dumps(
                        {"type": "termination", "timestamp": int(time.time() * 1000)}
                    ),
                )
                time.sleep(0.1)  # Small delay between termination signals
        except Exception as e:
            print(f"Error sending termination signal to {channel}: {e}")

    # Allow time for final messages to be processed
    print("Waiting for final messages to be processed...")
    time.sleep(3)

    # Print test results
    print_results()


if __name__ == "__main__":
    try:
        run_hardcore_test()
    except Exception as e:
        print(f"Critical test error: {e}")
        import sys

        sys.exit(1)
