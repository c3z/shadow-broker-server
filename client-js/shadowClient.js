import WebSocket from 'ws';
import fetch from 'node-fetch';
import process from 'process';
import base64 from 'base-64';

/**
 * Loads the broker configuration from SHADOW_BROKER_CONFIG,
 * which should be a base64-encoded JSON string.
 */
function loadConfig() {
  const b64 = process.env.SHADOW_BROKER_CONFIG;
  if (!b64) {
    throw new Error('Missing SHADOW_BROKER_CONFIG in the environment');
  }

  try {
    const decoded = base64.decode(b64);
    const config = JSON.parse(decoded);

    // Validate required fields
    if (!config.url) throw new Error('Missing url in config');
    if (!config.auth_token) throw new Error('Missing auth_token in config');

    return config;
  } catch (err) {
    if (err instanceof SyntaxError) {
      throw new Error(`Invalid JSON in SHADOW_BROKER_CONFIG: ${err.message}`);
    }
    throw new Error(`Failed to parse config: ${err.message}`);
  }
}

export class ShadowClient {
  constructor() {
    this.conf = loadConfig();
    this.isConnected = false;
    this.reconnectAttempts = 0;
    this.maxReconnectAttempts = 5;
    this.reconnectDelay = 1000; // Start with 1s delay
  }

  /**
   * Publish a message to a given queue.
   * @param {string} queue - The name of the queue
   * @param {string} message - The message content
   * @returns {Promise<string>} - The ID of the published message
   */
  /**
   * Publish a message. options.lane === 'control' sets X-Broker-Lane (separate
   * rate-limit budget on the broker); dispatch priority comes from the queue
   * name ending in '.control'.
   */
  async publish(queue, message, options = {}) {
    if (!queue || typeof queue !== 'string') {
      throw new Error('Queue name must be a non-empty string');
    }

    if (!message || typeof message !== 'string') {
      throw new Error('Message must be a non-empty string');
    }

    const pubUrl = this.conf.url.replace(/\/$/, '') + '/publish';

    try {
      const resp = await fetch(pubUrl, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Auth': this.conf.auth_token,
          ...(options.lane ? { 'X-Broker-Lane': options.lane } : {}),
        },
        body: JSON.stringify({ queue, message }),
        timeout: 10000, // 10s timeout
      });

      if (!resp.ok) {
        const errorText = await resp.text().catch(() => 'Unknown error');
        throw new Error(`Publish failed: ${resp.status} ${resp.statusText} - ${errorText}`);
      }

      const result = await resp.json();
      if (!result || !result.id) {
        throw new Error('Invalid response from server: missing message ID');
      }

      console.log(`JS => Published to queue "${queue}" with ID: ${result.id}`);
      return result.id;
    } catch (err) {
      if (err.name === 'AbortError') {
        throw new Error('Publish request timed out');
      }
      if (err.code === 'ECONNREFUSED' || err.code === 'ENOTFOUND') {
        throw new Error(`Cannot connect to broker at ${this.conf.url}: ${err.message}`);
      }
      throw err; // Re-throw other errors
    }
  }

  /**
   * Subscribe to a queue via WebSocket. Messages are delivered to onMessage callback.
   * @param {string} queue - The name of the queue
   * @param {(message: string) => void} onMessage - Callback for inbound messages
   * @param {Object} options - Optional configuration
   * @param {boolean} options.autoReconnect - Whether to auto-reconnect on disconnect (default: true)
   * @returns The WebSocket object
   */
  subscribe(queue, onMessage, options = { autoReconnect: true }) {
    if (!queue || typeof queue !== 'string') {
      throw new Error('Queue name must be a non-empty string');
    }

    if (typeof onMessage !== 'function') {
      throw new Error('onMessage must be a function');
    }

    const wsUrl = this.conf.url.replace(/^http/, 'ws').replace(/\/$/, '') + `/ws?queue=${encodeURIComponent(queue)}`;

    const ws = new WebSocket(wsUrl);
    let heartbeatInterval;

    const setupHeartbeat = () => {
      // Clear any existing interval
      if (heartbeatInterval) clearInterval(heartbeatInterval);

      // Set up a new heartbeat
      heartbeatInterval = setInterval(() => {
        if (ws.readyState === WebSocket.OPEN) {
          try {
            ws.send(JSON.stringify({ type: 'ping' }));
          } catch (err) {
            console.error(`JS => Error sending heartbeat: ${err.message}`);
          }
        }
      }, 30000); // 30s heartbeat
    };

    ws.on('open', () => {
      console.log(`JS => WebSocket open for queue: ${queue}, sending auth token...`);
      ws.send(JSON.stringify({ token: this.conf.auth_token }));
      this.isConnected = true;
      this.reconnectAttempts = 0;
      this.reconnectDelay = 1000; // Reset delay on successful connection
      setupHeartbeat();
    });

    ws.on('close', () => {
      console.log(`JS => WebSocket closed for queue: ${queue}`);
      this.isConnected = false;
      clearInterval(heartbeatInterval);

      if (options.autoReconnect && this.reconnectAttempts < this.maxReconnectAttempts) {
        this.reconnectAttempts++;
        console.log(`JS => Attempting to reconnect (${this.reconnectAttempts}/${this.maxReconnectAttempts}) in ${this.reconnectDelay}ms...`);

        setTimeout(() => {
          try {
            this.subscribe(queue, onMessage, options);
          } catch (err) {
            console.error(`JS => Reconnect failed: ${err.message}`);
          }
        }, this.reconnectDelay);

        // Exponential backoff with jitter
        this.reconnectDelay = Math.min(30000, this.reconnectDelay * 1.5 + Math.random() * 1000);
      } else if (this.reconnectAttempts >= this.maxReconnectAttempts) {
        console.error(`JS => Max reconnect attempts (${this.maxReconnectAttempts}) reached for queue: ${queue}`);
      }
    });

    ws.on('error', (err) => {
      console.error(`JS => WebSocket error for queue ${queue}:`, err.message);
    });

    ws.on('message', (data) => {
      try {
        const parsed = JSON.parse(data);
        if (parsed.type === 'message') {
          if (parsed.message !== undefined) {
            onMessage(parsed.message);
          } else {
            console.warn('JS => Received message event with undefined message content');
          }
        } else if (parsed.type === 'pong') {
          console.log('JS => Received PONG');
        } else if (parsed.type === 'error') {
          console.error(`JS => Received error from server: ${parsed.message || 'Unknown error'}`);
        }
      } catch (err) {
        console.error(`JS => Error parsing WS message: ${err.message}`);
      }
    });

    return ws;
  }

  /**
   * Pull messages from a queue via HTTP.
   * @param {string} queue - The name of the queue
   * @param {Object} options - Optional configuration
   * @param {number} options.since - Only return messages with ID > since (default: 0)
   * @param {number} options.limit - Maximum number of messages to return (default: 100, max: 1000)
   * @returns {Promise<{messages: Array<{id: number, message: string, created_at: string}>, last_id: number}>}
   */
  async pull(queue, options = {}) {
    if (!queue || typeof queue !== 'string') {
      throw new Error('Queue name must be a non-empty string');
    }

    const since = options.since || 0;
    const limit = options.limit || 100;

    if (typeof since !== 'number' || since < 0) {
      throw new Error('since must be a non-negative number');
    }

    if (typeof limit !== 'number' || limit <= 0 || limit > 1000) {
      throw new Error('limit must be a positive number up to 1000');
    }

    const pullUrl = new URL('/pull', this.conf.url);
    pullUrl.searchParams.append('queue', queue);
    pullUrl.searchParams.append('since', since.toString());
    pullUrl.searchParams.append('limit', limit.toString());

    try {
      const resp = await fetch(pullUrl.toString(), {
        method: 'GET',
        headers: {
          'X-Auth': this.conf.auth_token,
        },
        timeout: 10000, // 10s timeout
      });

      if (!resp.ok) {
        const errorText = await resp.text().catch(() => 'Unknown error');
        throw new Error(`Pull failed: ${resp.status} ${resp.statusText} - ${errorText}`);
      }

      const result = await resp.json();
      if (!result || !Array.isArray(result.messages) || typeof result.last_id !== 'number') {
        throw new Error('Invalid response from server');
      }

      console.log(`JS => Pulled ${result.messages.length} messages from queue "${queue}" (since=${since}, limit=${limit})`);
      return result;
    } catch (err) {
      if (err.name === 'AbortError') {
        throw new Error('Pull request timed out');
      }
      if (err.code === 'ECONNREFUSED' || err.code === 'ENOTFOUND') {
        throw new Error(`Cannot connect to broker at ${this.conf.url}: ${err.message}`);
      }
      throw err; // Re-throw other errors
    }
  }

  /**
   * Sends a ping through a temporary ephemeral WebSocket,
   * expecting a "pong" response.
   * @returns {Promise<boolean>} - Resolves to true if pong received, false otherwise
   */
  ping() {
    return new Promise((resolve, reject) => {
      // Minimal hack: open a short-lived WS to /ws?queue=_ping and send {"type":"ping"}.
      const wsUrl = this.conf.url.replace(/^http/, 'ws').replace(/\/$/, '') + `/ws?queue=_ping`;

      let pongReceived = false;
      let timeoutId;

      try {
        const ws = new WebSocket(wsUrl);

        // Set a timeout for the entire operation
        timeoutId = setTimeout(() => {
          if (!pongReceived) {
            console.error('JS => Ping timed out after 5 seconds');
            ws.close();
            resolve(false);
          }
        }, 5000);

        ws.on('open', () => {
          console.log('JS => Ping WS opened, sending auth token...');
          try {
            ws.send(JSON.stringify({ token: this.conf.auth_token }));
            ws.send(JSON.stringify({ type: 'ping' }));
          } catch (err) {
            console.error(`JS => Error sending ping: ${err.message}`);
            clearTimeout(timeoutId);
            ws.close();
            resolve(false);
          }
        });

        ws.on('error', (err) => {
          console.error(`JS => Ping WS error: ${err.message}`);
          clearTimeout(timeoutId);
          resolve(false);
        });

        ws.on('message', (data) => {
          try {
            const parsed = JSON.parse(data);
            if (parsed.type === 'pong') {
              console.log('JS => Received PONG from ephemeral WS');
              pongReceived = true;
              clearTimeout(timeoutId);
              ws.close();
              resolve(true);
            }
          } catch (err) {
            console.error(`JS => Error parsing ping response: ${err.message}`);
          }
        });

        ws.on('close', () => {
          clearTimeout(timeoutId);
          if (!pongReceived) {
            resolve(false);
          }
        });
      } catch (err) {
        console.error(`JS => Failed to create ping WebSocket: ${err.message}`);
        clearTimeout(timeoutId);
        reject(err);
      }
    });
  }
}

// Example usage if this file is run directly:
if (import.meta.url === `file://${process.argv[1]}`) {
  (async () => {
    try {
      const client = new ShadowClient();

      // Subscribe to a queue:
      const ws = client.subscribe('demo', (message) => {
        console.log("JS => Incoming message on 'demo':", message);
      });

      // Add error handler for the example
      ws.on('error', (err) => {
        console.error('JS => WebSocket error in example:', err.message);
      });

      try {
        // Publish a message:
        const msgId = await client.publish('demo', 'Hello from JavaScript!');
        console.log('JS => Published message ID:', msgId);
      } catch (pubError) {
        console.error('JS => Failed to publish message:', pubError.message);
      }

      try {
        // Pull messages:
        console.log('JS => Pulling messages...');
        const pullResult = await client.pull('demo', { since: 0, limit: 10 });
        console.log(`JS => Pulled ${pullResult.messages.length} messages, last_id: ${pullResult.last_id}`);

        // Print pulled messages
        if (pullResult.messages.length > 0) {
          pullResult.messages.forEach((msg) => {
            console.log(`JS => Message ID=${msg.id}, Content: ${msg.message}`);
          });
        }
      } catch (pullError) {
        console.error('JS => Pull error:', pullError.message);
      }

      try {
        // Ping:
        console.log('JS => Sending ping...');
        const pingResult = await client.ping();
        console.log(`JS => Ping result: ${pingResult ? 'Success' : 'Failed'}`);
      } catch (pingError) {
        console.error('JS => Ping error:', pingError.message);
      }

      // Wait 5 seconds and exit
      setTimeout(() => {
        console.log('JS => Finished');
        process.exit(0);
      }, 5000);
    } catch (error) {
      console.error('JS => Error in main example usage:', error);
      process.exit(1);
    }
  })();
}
