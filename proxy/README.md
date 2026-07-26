# Shadow Broker Webhook Proxy Server

A flexible webhook proxy server that subscribes to Shadow Broker channels and forwards messages to configured webhook endpoints.

## Overview

This service creates a bridge between Shadow Broker channels and HTTP webhook endpoints. It subscribes to configured channels and forwards any received messages to designated webhook URLs using either GET or POST methods.

## Installation

```bash
# Clone the repository
git clone <repository-url>

# Install dependencies
npm install

# Configure the service
cp config.example.yaml config.yaml
# Edit config.yaml with your settings

# Start the service
npm start
```

## Configuration

Configuration is stored in a YAML file (default: `config.yaml` in the same directory as the server). You can specify a different config file using the `CONFIG_FILE` environment variable.

### Sample Configuration

```yaml
shadowBrokerConfig: path/to/shadow/broker/config
server:
  port: 3000
  logLevel: info
mappings:
  - channel: channel-name-1
    webhook: https://example.com/webhook1
    method: GET
    parseJson: true
  - channel: channel-name-2
    webhook: https://example.com/webhook2
    method: POST
    parseJson: false
```

### Configuration Options

- `shadowBrokerConfig`: Path to Shadow Broker client configuration
- `admin_token`: Token protecting the web UI and all `/api/*` endpoints (see [Authentication](#authentication))
- `server.port`: HTTP server port (default: 3000)
- `server.logLevel`: Logging level (default: info)
- `mappings`: Array of channel-to-webhook mappings

### Mapping Options

Each mapping must include:

- `channel`: The Shadow Broker channel to subscribe to
- `webhook`: The URL to forward messages to

Optional mapping parameters:

- `method`: HTTP method to use (GET or POST, default: GET)
- `parseJson`: Whether to attempt parsing messages as JSON (default: true)

## How It Works

### Message Forwarding

1. **Subscribe to Channels**: The server subscribes to each configured channel using the Shadow Broker client.

2. **Message Reception**: When a message is received on a channel, it's processed according to the mapping configuration.

3. **JSON Parsing (if enabled)**:

   - If `parseJson: true`, the server attempts to parse the message as JSON
   - If parsing succeeds, it sets Content-Type to `application/json`
   - If parsing fails, it keeps the raw message as plain text with Content-Type `text/plain`

4. **HTTP Request Formation**:

   - For **GET** requests:

     - If the payload is a JSON object, each key-value pair is added as query parameters
     - Nested objects are stringified and URL-encoded
     - If the payload is a string, it's added as a `message` query parameter
     - All values are properly URL-encoded to handle special characters

   - For **POST** requests:
     - If the payload is a JSON object, it's stringified and sent with Content-Type `application/json`
     - If the payload is a string, it's sent as-is with Content-Type `text/plain`
     - The raw body contains the message content

5. **HTTP Headers**: All requests include:

   - `X-Shadow-Broker-Channel`: Name of the source channel
   - Appropriate `Content-Type` header based on the content

6. **Response Handling**: Successful deliveries and errors are logged

### Special Character Handling

All parameters and values are properly escaped using `encodeURIComponent()` to ensure special characters don't cause issues:

- For GET requests, query parameters are safely URL-encoded
- For POST requests with JSON payloads, the JSON stringification handles escaping

## Authentication

The web UI (`GET /`) and all `/api/*` endpoints can be protected with an admin token. The `/health` endpoint always stays public (it is used by the Docker healthcheck).

Set the token in `config.yaml`:

```yaml
admin_token: "your-secret-token"
```

If `admin_token` is empty or missing in the config file, the `PROXY_ADMIN_TOKEN` environment variable is used as a fallback. If both are empty, the API and UI are left unprotected and a warning is logged at startup.

Clients pass the token either as an `Authorization: Bearer <token>` header or as a `?token=<token>` query parameter:

```bash
curl -H "Authorization: Bearer your-secret-token" http://localhost:3000/api/mappings
curl "http://localhost:3000/api/config?token=your-secret-token"
```

The web UI asks for the token on first visit (stored in the browser's `localStorage`) and attaches it automatically to API requests and the Socket.IO connection. Use the **Reset token** button in the header to clear it and re-enter. Without a valid token the API responds with `401 Unauthorized`.

## API Endpoints

The server provides a web UI and REST API for management:

### Web Interface

- `GET /`: Web UI for managing the proxy

### REST API

#### Configuration

- `GET /api/config`: Get the current configuration as YAML
- `POST /api/config`: Update the configuration (body: YAML string)

#### Mappings

- `GET /api/mappings`: List all channel-webhook mappings
- `POST /api/mappings`: Create a new mapping
  ```json
  {
    "channel": "channel-name",
    "webhook": "https://example.com/webhook",
    "method": "POST",
    "parseJson": true
  }
  ```
- `DELETE /api/mappings/:index`: Delete a mapping by index

#### System

- `GET /health`: System health status
- `POST /api/restart`: Restart the proxy service

## Troubleshooting

### Common Issues

1. **Messages not being forwarded**

   - Check the logs for connection issues
   - Verify the Shadow Broker configuration is correct
   - Ensure the webhook URL is accessible from the server

2. **JSON parsing errors**

   - If messages aren't valid JSON, set `parseJson: false` or fix the message format
   - Check logs for specific parsing errors

3. **Missing payload in webhook**
   - For GET requests, check that query parameters contain the data
   - For POST requests, ensure the endpoint is reading the correct body format

### Logs

Logs are stored in:

- Console output
- `logs/combined.log`: All logs
- `logs/error.log`: Error logs only

Logs include detailed information about connections, message receipt, and delivery attempts.

## WebSocket Status

The server maintains a WebSocket connection for each subscription and monitors their status. You can see the status of all connections in the web UI or via the `/health` endpoint.
