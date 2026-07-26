// Admin token management (stored in localStorage, prompted when missing)
function getAdminToken() {
  let token = localStorage.getItem('admin_token');
  if (!token) {
    token = prompt('Enter admin token:');
    if (token) localStorage.setItem('admin_token', token);
  }
  return token || '';
}

// Clear the stored token and reload (re-prompts on next load)
function resetAdminToken() {
  localStorage.removeItem('admin_token');
  location.reload();
}

const adminToken = getAdminToken();

// Build an API URL including the admin token
function apiUrl(path) {
  if (!adminToken) return path;
  const sep = path.includes('?') ? '&' : '?';
  return `${path}${sep}token=${encodeURIComponent(adminToken)}`;
}

// Handle unauthorized responses by clearing the stored token and reloading
function handleAuthError(response) {
  if (response.status === 401) {
    localStorage.removeItem('admin_token');
    showNotification('Unauthorized - please re-enter the admin token', true);
    setTimeout(() => location.reload(), 1000);
    return true;
  }
  return false;
}

// Connect to Socket.IO
const socket = io({ auth: { token: adminToken } });

// DOM Elements
const logsContainer = document.getElementById('logs');
const mappingsTable = document.getElementById('mappings-table');
const mappingsBody = document.getElementById('mappings-body');
const addMappingForm = document.getElementById('add-mapping-form');
const configEditor = document.getElementById('config-editor');
const saveConfigBtn = document.getElementById('save-config');
const restartProxyBtn = document.getElementById('restart-proxy');
const connectionStatus = document.getElementById('connection-status');

// Track current subscription status
let subscriptions = [];

// Initialize the app
function init() {
  // Load initial data
  fetchMappings();
  fetchConfig();

  // Setup event listeners
  setupSocketEvents();
  setupFormListeners();
}

// Setup Socket.IO event listeners
function setupSocketEvents() {
  // Connection status
  socket.on('connect', () => {
    connectionStatus.innerHTML = '<span class="status-indicator status-connected"></span> Connected';
  });

  socket.on('disconnect', () => {
    connectionStatus.innerHTML = '<span class="status-indicator status-disconnected"></span> Disconnected';
  });

  // Auth failures (e.g. invalid token in handshake)
  socket.on('connect_error', (error) => {
    if (error && /unauthorized/i.test(error.message || '')) {
      localStorage.removeItem('admin_token');
      showNotification('Unauthorized - please re-enter the admin token', true);
      setTimeout(() => location.reload(), 1000);
    }
  });

  // Log events
  socket.on('log', (logEntry) => {
    appendLog(logEntry);
  });

  // Status updates
  socket.on('subscriptions', (data) => {
    subscriptions = data;
    updateMappingsTable();
  });
}

// Setup form submission listeners
function setupFormListeners() {
  // Add new mapping form
  if (addMappingForm) {
    addMappingForm.addEventListener('submit', (e) => {
      e.preventDefault();

      const channel = document.getElementById('channel').value;
      const webhook = document.getElementById('webhook').value;
      const method = document.getElementById('method').value;
      const parseJson = document.getElementById('parseJson').checked;

      addMapping({ channel, webhook, method, parseJson });
    });
  }

  // Save config button
  if (saveConfigBtn) {
    saveConfigBtn.addEventListener('click', () => {
      saveConfig(configEditor.value);
    });
  }

  // Restart proxy button
  if (restartProxyBtn) {
    restartProxyBtn.addEventListener('click', () => {
      restartProxy();
    });
  }
}

// Fetch current mappings
function fetchMappings() {
  fetch(apiUrl('/api/mappings'))
    .then((response) => {
      if (handleAuthError(response)) return;
      return response.json();
    })
    .then((data) => {
      if (data) updateMappingsTable(data);
    })
    .catch((error) => {
      showNotification('Failed to load mappings', true);
      console.error('Error fetching mappings:', error);
    });
}

// Fetch the current YAML config
function fetchConfig() {
  fetch(apiUrl('/api/config'))
    .then((response) => {
      if (handleAuthError(response)) return;
      return response.text();
    })
    .then((data) => {
      if (data !== undefined) configEditor.value = data;
    })
    .catch((error) => {
      showNotification('Failed to load configuration', true);
      console.error('Error fetching config:', error);
    });
}

// Add a new mapping
function addMapping(mapping) {
  fetch(apiUrl('/api/mappings'), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(mapping),
  })
    .then((response) => {
      if (handleAuthError(response)) return;
      if (!response.ok) {
        throw new Error('Failed to add mapping');
      }
      return response.json();
    })
    .then((data) => {
      if (!data) return;
      showNotification('Mapping added successfully');
      fetchMappings();
      // Reset form
      addMappingForm.reset();
    })
    .catch((error) => {
      showNotification('Failed to add mapping: ' + error.message, true);
      console.error('Error adding mapping:', error);
    });
}

// Delete a mapping
function deleteMapping(index) {
  fetch(apiUrl(`/api/mappings/${index}`), {
    method: 'DELETE',
  })
    .then((response) => {
      if (handleAuthError(response)) return;
      if (!response.ok) {
        throw new Error('Failed to delete mapping');
      }
      return response.json();
    })
    .then((data) => {
      if (!data) return;
      showNotification('Mapping deleted successfully');
      fetchMappings();
    })
    .catch((error) => {
      showNotification('Failed to delete mapping', true);
      console.error('Error deleting mapping:', error);
    });
}

// Save YAML config
function saveConfig(yamlConfig) {
  // Show a loading state on the button
  if (saveConfigBtn) {
    const originalText = saveConfigBtn.textContent;
    saveConfigBtn.textContent = 'Saving...';
    saveConfigBtn.disabled = true;
  }

  fetch(apiUrl('/api/config'), {
    method: 'POST',
    headers: {
      'Content-Type': 'text/plain', // YAML content
    },
    body: yamlConfig,
  })
    .then((response) => {
      if (handleAuthError(response)) return;
      if (!response.ok) {
        throw new Error('Failed to save configuration');
      }
      return response.json();
    })
    .then((data) => {
      if (!data) return;
      showNotification('Configuration saved successfully');

      // Refresh mappings after save
      fetchMappings();
    })
    .catch((error) => {
      showNotification('Failed to save configuration: ' + error.message, true);
      console.error('Error saving config:', error);
    })
    .finally(() => {
      // Reset button state
      if (saveConfigBtn) {
        saveConfigBtn.textContent = 'Save Configuration';
        saveConfigBtn.disabled = false;
      }
    });
}

// Update the mappings table in the UI
function updateMappingsTable(mappings = null) {
  if (!mappingsBody) return;

  // Clear the table
  mappingsBody.innerHTML = '';

  // If mappings provided, use them, otherwise use from last status update
  const data = mappings || subscriptions;

  if (!data || data.length === 0) {
    const row = document.createElement('tr');
    row.innerHTML = '<td colspan="5" class="text-center">No mappings configured</td>';
    mappingsBody.appendChild(row);
    return;
  }

  // Add each mapping to the table
  data.forEach((mapping, index) => {
    const row = document.createElement('tr');

    // Status indicator
    const isConnected = mapping.connected !== false;
    const statusHtml = isConnected ? '<span class="status-indicator status-connected"></span> Connected' : '<span class="status-indicator status-disconnected"></span> Disconnected';

    row.innerHTML = `
      <td>${mapping.channel}</td>
      <td>${mapping.webhook}</td>
      <td>${mapping.method || 'GET'}</td>
      <td><div class="status-container">${statusHtml}</div></td>
      <td class="actions">
        <button class="btn btn-danger btn-sm" onclick="deleteMapping(${index})">Delete</button>
      </td>
    `;

    mappingsBody.appendChild(row);
  });
}

// Append a log entry to the logs container
function appendLog(logEntry) {
  if (!logsContainer) return;

  const logElement = document.createElement('div');
  logElement.className = `log-entry log-${logEntry.level || 'info'}`;

  // Format timestamp
  const timestamp = new Date(logEntry.timestamp || Date.now()).toISOString();

  // Format log message
  logElement.textContent = `[${timestamp}] [${logEntry.level || 'INFO'}] ${logEntry.message}`;

  // Add to logs container and scroll to bottom
  logsContainer.appendChild(logElement);
  logsContainer.scrollTop = logsContainer.scrollHeight;

  // Keep only the last 1000 log entries to prevent browser slowdown
  if (logsContainer.children.length > 1000) {
    logsContainer.removeChild(logsContainer.children[0]);
  }
}

// Show a notification
function showNotification(message, isError = false) {
  const notification = document.createElement('div');
  notification.className = `notification ${isError ? 'error' : ''}`;
  notification.textContent = message;

  document.body.appendChild(notification);

  // Show the notification (after small delay to ensure transition works)
  setTimeout(() => {
    notification.classList.add('show');
  }, 10);

  // Hide and remove after 3 seconds
  setTimeout(() => {
    notification.classList.remove('show');
    setTimeout(() => {
      notification.remove();
    }, 300); // Wait for the hide transition
  }, 3000);
}

// Restart the proxy service
function restartProxy() {
  // Show a loading state on the button
  if (restartProxyBtn) {
    const originalText = restartProxyBtn.textContent;
    restartProxyBtn.textContent = 'Restarting...';
    restartProxyBtn.disabled = true;
  }

  fetch(apiUrl('/api/restart'), {
    method: 'POST',
  })
    .then((response) => {
      if (handleAuthError(response)) return;
      if (!response.ok) {
        throw new Error('Failed to restart proxy');
      }
      return response.json();
    })
    .then((data) => {
      if (!data) return;
      showNotification('Proxy restarted successfully');

      // Refresh mappings after restart
      fetchMappings();
    })
    .catch((error) => {
      showNotification('Failed to restart proxy: ' + error.message, true);
      console.error('Error restarting proxy:', error);
    })
    .finally(() => {
      // Reset button state
      if (restartProxyBtn) {
        restartProxyBtn.textContent = 'Restart Proxy';
        restartProxyBtn.disabled = false;
      }
    });
}

// Initialize on document load
document.addEventListener('DOMContentLoaded', init);
