import { ShadowClient } from './shadowClient.js';
import express from 'express';
import fetch from 'node-fetch';
import fs from 'fs/promises';
import path from 'path';
import yaml from 'js-yaml';
import http from 'http';
import { Server as SocketIO } from 'socket.io';
import winston from 'winston';
import bodyParser from 'body-parser';
import { fileURLToPath } from 'url';

// Get directory name in ESM
const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Configuration file path
const CONFIG_FILE = process.env.CONFIG_FILE || path.join(__dirname, 'config.yaml');
const DATA_DIR = process.env.DATA_DIR || path.join(__dirname, 'data');
const LOGS_DIR = process.env.LOGS_DIR || path.join(__dirname, 'logs');

// Ensure directories exist
async function ensureDirExists(dir) {
  try {
    await fs.mkdir(dir, { recursive: true });
  } catch (error) {
    console.error(`Failed to create directory ${dir}:`, error);
  }
}

// Setup logging
const logger = winston.createLogger({
  level: 'info',
  format: winston.format.combine(winston.format.timestamp(), winston.format.json()),
  transports: [
    new winston.transports.Console(),
    new winston.transports.File({
      filename: path.join(LOGS_DIR, 'error.log'),
      level: 'error',
    }),
    new winston.transports.File({
      filename: path.join(LOGS_DIR, 'combined.log'),
    }),
  ],
});

// Setup express app and socket.io
const app = express();
const server = http.createServer(app);
const io = new SocketIO(server);

// Middleware
app.use(express.json());
app.use(express.static(path.join(__dirname, 'public')));
app.use(bodyParser.text({ type: 'text/plain' }));

// Set the view engine to ejs
app.set('view engine', 'ejs');
app.set('views', path.join(__dirname, 'views'));

// State variables
let config = {};
let shadowClient;
let activeSubscriptions = [];

// Resolve the admin token: config takes precedence, env variable as fallback
function getAdminToken() {
  return String(config.admin_token || process.env.PROXY_ADMIN_TOKEN || '').trim();
}

// Middleware protecting the UI and API routes with the admin token.
// Token can be passed as "Authorization: Bearer <token>" header or "?token=" query param.
function requireAdmin(req, res, next) {
  const token = getAdminToken();
  if (!token) return next(); // No token configured - open access (warning logged at startup)

  const authHeader = req.headers.authorization || '';
  const provided = authHeader.startsWith('Bearer ') ? authHeader.slice(7) : req.query.token;

  if (provided === token) return next();

  if (req.path.startsWith('/api/')) {
    return res.status(401).json({ error: 'Unauthorized: valid admin token required' });
  }
  res.status(401).send('<h1>401 Unauthorized</h1><p>Valid admin token required. Append ?token=YOUR_TOKEN to the URL.</p>');
}

// Load configuration from YAML file
async function loadConfig() {
  try {
    const configContent = await fs.readFile(CONFIG_FILE, 'utf8');
    config = yaml.load(configContent);
    logger.info('Configuration loaded successfully');
    return config;
  } catch (error) {
    if (error.code === 'ENOENT') {
      logger.warn(`Config file not found at ${CONFIG_FILE}, using defaults`);
      return {
        shadowBrokerConfig: process.env.SHADOW_BROKER_CONFIG,
        server: {
          port: process.env.PORT || 3000,
          logLevel: 'info',
        },
        mappings: [],
      };
    }
    logger.error(`Failed to load config: ${error.message}`);
    throw error;
  }
}

// Save configuration to YAML file
async function saveConfig(configData) {
  try {
    // If string is provided, parse it first (for UI editor)
    if (typeof configData === 'string') {
      configData = yaml.load(configData);
    }

    // Validate config
    if (!configData.shadowBrokerConfig) {
      throw new Error('Missing shadowBrokerConfig in configuration');
    }

    // Set environment variable for shadow client to use
    process.env.SHADOW_BROKER_CONFIG = configData.shadowBrokerConfig;

    // Update the log level if it's changed
    if (configData.server && configData.server.logLevel) {
      logger.level = configData.server.logLevel;
    }

    // Save to file as YAML
    const yamlString = yaml.dump(configData);
    await fs.writeFile(CONFIG_FILE, yamlString, 'utf8');

    // Update local config
    config = configData;

    logger.info('Configuration saved successfully');
    return { success: true };
  } catch (error) {
    logger.error(`Failed to save config: ${error.message}`);
    throw error;
  }
}

// Initialize the Shadow Client
async function initializeShadowClient() {
  try {
    // Close existing client if any
    if (shadowClient) {
      logger.info('Closing existing Shadow client connections...');

      // Close any active subscriptions
      for (const sub of activeSubscriptions) {
        try {
          logger.info(`Closing connection for channel ${sub.channel}`);
          sub.ws.close();

          // Small delay to ensure clean close
          await new Promise((resolve) => setTimeout(resolve, 100));
        } catch (error) {
          logger.error(`Error closing WebSocket for channel ${sub.channel}:`, error.message);
        }
      }

      activeSubscriptions = [];
      logger.info('All existing connections closed');
    }

    // Create new shadow client
    logger.info('Creating new Shadow client instance');
    shadowClient = new ShadowClient();
    logger.info('Shadow client initialized successfully');

    // Initialize mappings from config
    if (config.mappings && Array.isArray(config.mappings)) {
      logger.info(`Initializing ${config.mappings.length} channel to webhook mappings`);
      config.mappings.forEach(setupChannelForwarding);
    } else {
      logger.warn('No channel to webhook mappings configured');
    }

    // Broadcast active subscriptions to connected clients
    broadcastStatus();

    return true;
  } catch (error) {
    logger.error(`Failed to initialize Shadow client: ${error.message}`);
    throw error;
  }
}

// Setup channel to webhook forwarding
function setupChannelForwarding(mapping) {
  const { channel, webhook, method = 'GET', parseJson = true } = mapping;

  if (!channel || !webhook) {
    logger.error('Invalid mapping configuration: Missing channel or webhook');
    return;
  }

  logger.info(`Setting up forwarding from channel "${channel}" to webhook "${webhook}"`);

  // Setup message handler
  const handleMessage = async (message) => {
    try {
      logger.info(`Received message from channel "${channel}": ${message}`);

      let payload = message;
      let contentType = 'text/plain';

      // Parse JSON if enabled
      if (parseJson) {
        try {
          payload = JSON.parse(message);
          contentType = 'application/json';
          logger.info(`Successfully parsed message as JSON from channel "${channel}"`);
        } catch (error) {
          logger.error(`Failed to parse message as JSON from channel ${channel}: ${error.message}`);
          // Continue with raw message as plain text
          payload = message;
          contentType = 'text/plain';
        }
      }

      // For GET requests, append payload as query parameters
      let targetUrl = webhook;
      if (method === 'GET') {
        const url = new URL(webhook);
        if (typeof payload === 'object' && payload !== null) {
          // Handle JSON objects by converting to query params
          Object.entries(payload).forEach(([key, value]) => {
            // Ensure proper encoding of object values
            const paramValue = typeof value === 'object' ? encodeURIComponent(JSON.stringify(value)) : encodeURIComponent(String(value));
            url.searchParams.append(key, paramValue);
          });
        } else {
          // Handle string payload by adding it as a 'message' parameter
          url.searchParams.append('message', encodeURIComponent(String(payload)));
        }
        targetUrl = url.toString();
      }

      // Forward the message to the webhook
      logger.info(`Forwarding message from channel "${channel}" to webhook "${targetUrl}" using ${method}`);

      const options = {
        method,
        headers: {
          'X-Shadow-Broker-Channel': channel,
        },
        timeout: 10000, // 10s timeout
      };

      // For POST requests, add the payload in the body
      if (method === 'POST') {
        options.headers['Content-Type'] = contentType;

        if (typeof payload === 'object' && payload !== null) {
          try {
            options.body = JSON.stringify(payload);
          } catch (error) {
            logger.error(`Error stringifying JSON payload: ${error.message}`);
            // Fallback to string representation
            options.body = String(payload);
            options.headers['Content-Type'] = 'text/plain';
          }
        } else {
          // Handle string payload
          options.body = String(payload);
        }
      }

      const response = await fetch(targetUrl, options);

      if (!response.ok) {
        const errorText = await response.text().catch(() => 'Unknown error');
        logger.error(`Webhook delivery failed for channel ${channel}: ${response.status} ${response.statusText} - ${errorText}`);
      } else {
        logger.info(`Successfully delivered message from channel "${channel}" to webhook "${targetUrl}"`);
      }
    } catch (error) {
      logger.error(`Error forwarding message from channel ${channel} to webhook ${webhook}: ${error.message}`);
    }
  };

  // Subscribe to the channel
  try {
    const ws = shadowClient.subscribe(channel, handleMessage);
    activeSubscriptions.push({
      channel,
      webhook,
      method,
      parseJson,
      ws,
    });
    logger.info(`Successfully subscribed to channel "${channel}"`);

    // Broadcast updated status
    broadcastStatus();
  } catch (error) {
    logger.error(`Failed to subscribe to channel ${channel}: ${error.message}`);
  }
}

// Broadcast active subscriptions status to all connected clients
function broadcastStatus() {
  // Format subscription status for UI
  const status = activeSubscriptions.map((sub) => ({
    channel: sub.channel,
    webhook: sub.webhook,
    method: sub.method,
    parseJson: sub.parseJson,
    connected: sub.ws.readyState === 1, // WebSocket.OPEN
  }));

  io.emit('subscriptions', status);
}

// Socket.IO authentication middleware
io.use((socket, next) => {
  const token = getAdminToken();
  if (!token) return next(); // No token configured - open access

  const provided = (socket.handshake.auth && socket.handshake.auth.token) || socket.handshake.query.token;
  if (provided === token) return next();

  next(new Error('Unauthorized: valid admin token required'));
});

// Socket.IO connection handling
io.on('connection', (socket) => {
  logger.info('UI client connected');

  // Send initial status
  socket.emit(
    'subscriptions',
    activeSubscriptions.map((sub) => ({
      channel: sub.channel,
      webhook: sub.webhook,
      method: sub.method,
      parseJson: sub.parseJson,
      connected: sub.ws.readyState === 1,
    }))
  );

  socket.on('disconnect', () => {
    logger.info('UI client disconnected');
  });
});

// Custom winston transport to send logs to Socket.IO clients
class SocketTransport extends winston.Transport {
  constructor(opts) {
    super(opts);
  }

  log(info, callback) {
    setImmediate(() => {
      this.emit('logged', info);
    });

    // Send log to all connected clients
    io.emit('log', {
      level: info.level,
      message: info.message,
      timestamp: info.timestamp,
    });

    callback();
  }
}

// Add Socket.IO transport to winston
logger.add(new SocketTransport());

// API Routes
app.use('/api', requireAdmin);

app.get('/', requireAdmin, (req, res) => {
  res.render('index');
});

app.get('/health', (req, res) => {
  res.json({
    status: 'ok',
    activeSubscriptions: activeSubscriptions.map((sub) => ({
      channel: sub.channel,
      webhook: sub.webhook,
      connected: sub.ws.readyState === 1,
    })),
  });
});

// API endpoint to get mappings
app.get('/api/mappings', (req, res) => {
  res.json(config.mappings || []);
});

// API endpoint to add a mapping
app.post('/api/mappings', async (req, res) => {
  try {
    const { channel, webhook, method = 'GET', parseJson = true } = req.body;

    // Validate input
    if (!channel || typeof channel !== 'string') {
      return res.status(400).json({ error: 'Channel must be a non-empty string' });
    }
    if (!webhook || typeof webhook !== 'string') {
      return res.status(400).json({ error: 'Webhook must be a non-empty string' });
    }

    // Add to config
    if (!config.mappings) config.mappings = [];
    config.mappings.push({ channel, webhook, method, parseJson });

    // Save config
    await saveConfig(config);

    // Setup new forwarding
    setupChannelForwarding({ channel, webhook, method, parseJson });

    res.json({ success: true, message: 'Mapping added successfully' });
  } catch (error) {
    logger.error(`Failed to add mapping: ${error.message}`);
    res.status(500).json({ error: `Failed to add mapping: ${error.message}` });
  }
});

// API endpoint to delete a mapping
app.delete('/api/mappings/:index', async (req, res) => {
  try {
    const index = parseInt(req.params.index, 10);

    // Validate input
    if (isNaN(index) || index < 0 || !config.mappings || index >= config.mappings.length) {
      return res.status(400).json({ error: 'Invalid mapping index' });
    }

    // Remove mapping
    const removedMapping = config.mappings.splice(index, 1)[0];

    // Save config
    await saveConfig(config);

    // Close the websocket if active
    const subIndex = activeSubscriptions.findIndex((sub) => sub.channel === removedMapping.channel && sub.webhook === removedMapping.webhook);

    if (subIndex !== -1) {
      try {
        activeSubscriptions[subIndex].ws.close();
        activeSubscriptions.splice(subIndex, 1);

        // Broadcast updated status
        broadcastStatus();
      } catch (error) {
        logger.error(`Error closing WebSocket for channel ${removedMapping.channel}:`, error.message);
      }
    }

    res.json({ success: true, message: 'Mapping deleted successfully' });
  } catch (error) {
    logger.error(`Failed to delete mapping: ${error.message}`);
    res.status(500).json({ error: `Failed to delete mapping: ${error.message}` });
  }
});

// API endpoint to get the config as YAML
app.get('/api/config', (req, res) => {
  try {
    const yamlString = yaml.dump(config);
    res.setHeader('Content-Type', 'text/plain');
    res.send(yamlString);
  } catch (error) {
    logger.error(`Failed to get config: ${error.message}`);
    res.status(500).json({ error: `Failed to get config: ${error.message}` });
  }
});

// API endpoint to restart the proxy
app.post('/api/restart', async (req, res) => {
  try {
    logger.info('Restart requested via API');

    // Force reload config from disk
    await cleanRestartProxy();

    res.json({ success: true, message: 'Proxy restarted successfully' });
  } catch (error) {
    logger.error(`Failed to restart proxy: ${error.message}`);
    res.status(500).json({ error: `Failed to restart proxy: ${error.message}` });
  }
});

// API endpoint to update the config
app.post('/api/config', async (req, res) => {
  try {
    const yamlString = req.body;

    // Parse and save config
    const newConfig = yaml.load(yamlString);
    await saveConfig(newConfig);

    // Reinitialize shadow client with new config
    await initializeShadowClient();

    res.json({ success: true, message: 'Configuration updated successfully' });
  } catch (error) {
    logger.error(`Failed to update config: ${error.message}`);
    res.status(500).json({ error: `Failed to update config: ${error.message}` });
  }
});

// Clean restart function that fully reloads everything
async function cleanRestartProxy() {
  logger.info('Performing clean restart of the proxy service');

  // Close all existing connections
  if (shadowClient) {
    for (const sub of activeSubscriptions) {
      try {
        logger.info(`Closing connection for channel ${sub.channel}`);
        sub.ws.close();
      } catch (error) {
        logger.error(`Error closing WebSocket for channel ${sub.channel}:`, error.message);
      }
    }

    // Clear subscriptions
    activeSubscriptions = [];

    // Clear the shadow client
    shadowClient = null;
  }

  // Force reload config from disk (ignore cached version)
  try {
    const configContent = await fs.readFile(CONFIG_FILE, 'utf8');
    config = yaml.load(configContent);
    logger.info('Configuration reloaded successfully from disk');

    // Update environment variables
    if (config.shadowBrokerConfig) {
      process.env.SHADOW_BROKER_CONFIG = config.shadowBrokerConfig;
    }

    // Update log level
    if (config.server && config.server.logLevel) {
      logger.level = config.server.logLevel;
    }
  } catch (error) {
    logger.error(`Failed to reload config from disk: ${error.message}`);
    throw error;
  }

  // Reinitialize shadow client with fresh config
  try {
    logger.info('Reinitializing Shadow client');
    shadowClient = new ShadowClient();

    // Initialize mappings from config
    if (config.mappings && Array.isArray(config.mappings)) {
      logger.info(`Initializing ${config.mappings.length} channel to webhook mappings`);
      config.mappings.forEach(setupChannelForwarding);
    } else {
      logger.warn('No channel to webhook mappings configured');
    }

    // Broadcast active subscriptions to connected clients
    broadcastStatus();

    logger.info('Proxy restart completed successfully');
    return true;
  } catch (error) {
    logger.error(`Failed to reinitialize Shadow client: ${error.message}`);
    throw error;
  }
}

// Start the application
async function startApp() {
  try {
    // Ensure directories exist
    await ensureDirExists(DATA_DIR);
    await ensureDirExists(LOGS_DIR);

    // Load config
    await loadConfig();

    // Warn when the API and UI are left unprotected
    if (!getAdminToken()) {
      logger.warn('WARNING: admin_token is not set - the API and web UI are NOT secured. Set admin_token in the config file or the PROXY_ADMIN_TOKEN environment variable.');
    }

    // Set log level from config
    if (config.server && config.server.logLevel) {
      logger.level = config.server.logLevel;
    }

    // Make sure Shadow Broker config is accessible
    process.env.SHADOW_BROKER_CONFIG = config.shadowBrokerConfig;

    // Initialize Shadow Client using our clean restart function
    await cleanRestartProxy();

    // Determine port
    const PORT = (config.server && config.server.port) || process.env.PORT || 3000;

    // Start the server
    server.listen(PORT, () => {
      logger.info(`Webhook proxy server listening on port ${PORT}`);

      // Periodically check for active connections
      setInterval(() => {
        const connectedCount = activeSubscriptions.filter((sub) => sub.ws.readyState === 1).length;
        logger.info(`Connection status: ${connectedCount}/${activeSubscriptions.length} active subscriptions`);

        // Broadcast status
        broadcastStatus();
      }, 60000); // Check every minute
    });
  } catch (error) {
    logger.error(`Failed to start application: ${error.message}`);
    process.exit(1);
  }
}

// Handle process shutdown
process.on('SIGINT', () => {
  logger.info('Shutting down webhook proxy...');
  activeSubscriptions.forEach((sub) => {
    try {
      sub.ws.close();
    } catch (error) {
      logger.error(`Error closing WebSocket for channel ${sub.channel}:`, error.message);
    }
  });
  process.exit(0);
});

process.on('SIGTERM', () => {
  logger.info('Termination signal received, shutting down...');
  process.exit(0);
});

// Start the application
startApp();
