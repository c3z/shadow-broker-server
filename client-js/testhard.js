import { ShadowClient } from './shadowClient.js';

// Configuration
const TEST_DURATION_MS = 60000; // 1 minute test
const CHANNELS = ['stress_small', 'stress_medium', 'stress_large', 'stress_order'];
const PUBLISHERS_PER_CHANNEL = 3;
const SUBSCRIBERS_PER_CHANNEL = 5;
const MESSAGE_RATES = {
  stress_small: 100, // msgs per second target (very high frequency)
  stress_medium: 50, // msgs per second target
  stress_large: 10, // msgs per second target (large messages)
  stress_order: 200, // msgs per second target (to test ordering)
};

// Statistics tracking
const stats = {
  published: { total: 0 },
  received: { total: 0 },
  missed: { total: 0 },
  latencies: [],
  errors: 0,
  outOfOrder: 0,
};

CHANNELS.forEach((channel) => {
  stats.published[channel] = 0;
  stats.received[channel] = 0;
});

async function runHardcoreTest() {
  console.log('Starting hardcore broker stress test...');

  // Create client instances for publishers and subscribers
  const publisherClients = [];
  const subscriberClients = [];

  // Create publisher clients
  for (let i = 0; i < CHANNELS.length * PUBLISHERS_PER_CHANNEL; i++) {
    publisherClients.push(new ShadowClient());
  }

  // Create subscriber clients
  for (let i = 0; i < CHANNELS.length * SUBSCRIBERS_PER_CHANNEL; i++) {
    subscriberClients.push(new ShadowClient());
  }

  // Allow connections to establish
  await new Promise((resolve) => setTimeout(resolve, 1000));

  // Setup subscribers
  setupSubscribers(subscriberClients);

  // Start publishing on all channels
  const publishPromises = [];
  let channelIndex = 0;

  for (let i = 0; i < publisherClients.length; i++) {
    const channel = CHANNELS[channelIndex];
    publishPromises.push(startPublishing(publisherClients[i], channel, i));

    channelIndex = (channelIndex + 1) % CHANNELS.length;
  }

  // Create timer to end test
  const testEndPromise = new Promise((resolve) => {
    setTimeout(() => {
      console.log('Test duration reached, sending termination signals...');
      resolve();
    }, TEST_DURATION_MS);
  });

  // Wait for test to complete
  await testEndPromise;

  // Send termination messages
  for (const channel of CHANNELS) {
    // Use first publisher client to send termination
    await publisherClients[0].publish(
      channel,
      JSON.stringify({
        type: 'termination',
        timestamp: Date.now(),
      })
    );
  }

  // Allow time for final messages to be processed
  await new Promise((resolve) => setTimeout(resolve, 3000));

  // Print test results
  printResults();
}

function setupSubscribers(clients) {
  let clientIndex = 0;

  for (const channel of CHANNELS) {
    for (let i = 0; i < SUBSCRIBERS_PER_CHANNEL; i++) {
      const client = clients[clientIndex++];
      const subscriberId = `${channel}_sub_${i}`;

      // Maintain received messages for order verification
      const receivedSequence = new Map();

      client.subscribe(channel, (message) => {
        try {
          const data = JSON.parse(message);

          // Handle termination message
          if (data.type === 'termination') {
            console.log(`${subscriberId}: Received termination signal`);
            return;
          }

          // Record message receipt
          stats.received.total++;
          stats.received[channel] = (stats.received[channel] || 0) + 1;

          // Calculate latency
          const latency = Date.now() - data.timestamp;
          stats.latencies.push(latency);

          // Check message ordering for the order test channel
          if (channel === 'stress_order') {
            const publisherId = data.publisherId;
            const sequence = data.sequence;

            if (!receivedSequence.has(publisherId)) {
              receivedSequence.set(publisherId, sequence);
            } else {
              const lastSequence = receivedSequence.get(publisherId);
              if (sequence !== lastSequence + 1) {
                stats.outOfOrder++;
                console.warn(`${subscriberId}: Out of order message from ${publisherId}. Expected ${lastSequence + 1}, got ${sequence}`);
              }
              receivedSequence.set(publisherId, sequence);
            }
          }

          // For large messages, just count them without logging content
          if (channel !== 'stress_large') {
            if (Math.random() < 0.001) {
              // Log only 0.1% of messages to avoid console spam
              console.log(`${subscriberId} received: ${JSON.stringify(data).substring(0, 50)}...`);
            }
          }
        } catch (err) {
          stats.errors++;
          console.error(`${subscriberId} error parsing message:`, err);
        }
      });

      console.log(`${subscriberId} started and listening to '${channel}'`);
    }
  }
}

async function startPublishing(client, channel, publisherIndex) {
  const publisherId = `${channel}_pub_${publisherIndex}`;
  let sequence = 0;
  let running = true;

  console.log(`Publisher ${publisherId} started on channel ${channel}`);

  // Calculate delay based on target message rate
  const targetDelayMs = 1000 / MESSAGE_RATES[channel];

  setTimeout(() => {
    running = false;
  }, TEST_DURATION_MS);

  while (running) {
    try {
      const timestamp = Date.now();
      let payload;

      // Create different message sizes based on channel
      switch (channel) {
        case 'stress_small':
          payload = {
            type: 'data',
            publisherId,
            sequence: sequence++,
            timestamp,
            value: Math.random(),
          };
          break;

        case 'stress_medium':
          payload = {
            type: 'data',
            publisherId,
            sequence: sequence++,
            timestamp,
            value: Math.random(),
            data: Array.from({ length: 100 }, () => Math.random().toString(36).substring(2)),
          };
          break;

        case 'stress_large':
          payload = {
            type: 'data',
            publisherId,
            sequence: sequence++,
            timestamp,
            value: Math.random(),
            // Create a large nested object
            data: Array.from({ length: 500 }, (_, i) => ({
              id: i,
              values: Array.from({ length: 20 }, () => Math.random()),
              text: Math.random().toString(36).substring(2).repeat(10),
            })),
          };
          break;

        case 'stress_order':
          payload = {
            type: 'data',
            publisherId,
            sequence: sequence++,
            timestamp,
            value: Math.random(),
          };
          break;
      }

      await client.publish(channel, JSON.stringify(payload));

      // Update statistics
      stats.published.total++;
      stats.published[channel] = (stats.published[channel] || 0) + 1;

      // Add jitter to make the test more realistic
      const jitter = Math.random() * targetDelayMs * 0.3;
      const delay = Math.max(1, targetDelayMs - jitter);

      // For burst testing, occasionally send a batch of messages with minimal delay
      if (Math.random() < 0.05) {
        // 5% chance of burst
        const burstCount = Math.floor(Math.random() * 10) + 5; // 5-15 messages in burst
        for (let i = 0; i < burstCount && running; i++) {
          const burstPayload = {
            type: 'burst',
            publisherId,
            sequence: sequence++,
            timestamp: Date.now(),
            burstId: i,
            burstTotal: burstCount,
          };

          await client.publish(channel, JSON.stringify(burstPayload));
          stats.published.total++;
          stats.published[channel] = (stats.published[channel] || 0) + 1;

          // Minimal delay between burst messages
          await new Promise((resolve) => setTimeout(resolve, 1));
        }
      }

      await new Promise((resolve) => setTimeout(resolve, delay));
    } catch (err) {
      stats.errors++;
      console.error(`${publisherId} error publishing:`, err);
      // Brief pause on error
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
}

function printResults() {
  // Calculate statistics
  const totalPublished = stats.published.total;
  const totalReceived = stats.received.total;
  const expectedReceived = totalPublished * SUBSCRIBERS_PER_CHANNEL;
  stats.missed.total = expectedReceived - totalReceived;

  // Calculate average latency
  const avgLatency = stats.latencies.length > 0 ? stats.latencies.reduce((sum, val) => sum + val, 0) / stats.latencies.length : 0;

  // Calculate min and max latency
  const minLatency = stats.latencies.length > 0 ? Math.min(...stats.latencies) : 0;
  const maxLatency = stats.latencies.length > 0 ? Math.max(...stats.latencies) : 0;

  // Per-channel statistics
  const channelStats = CHANNELS.map((channel) => {
    const published = stats.published[channel] || 0;
    const received = stats.received[channel] || 0;
    const expectedChannelReceived = published * SUBSCRIBERS_PER_CHANNEL;
    const missed = expectedChannelReceived - received;
    const deliveryRate = expectedChannelReceived > 0 ? ((received / expectedChannelReceived) * 100).toFixed(2) : '0.00';

    return {
      channel,
      published,
      received,
      expectedReceived: expectedChannelReceived,
      missed,
      deliveryRate: `${deliveryRate}%`,
    };
  });

  console.log('\n========== HARDCORE BROKER TEST RESULTS ==========');
  console.log(`Test duration: ${TEST_DURATION_MS / 1000} seconds`);
  console.log(`Total messages published: ${totalPublished}`);
  console.log(`Expected message deliveries: ${expectedReceived}`);
  console.log(`Total messages received: ${totalReceived}`);
  console.log(`Missed deliveries: ${stats.missed.total}`);
  console.log(`Delivery rate: ${((totalReceived / expectedReceived) * 100).toFixed(2)}%`);
  console.log(`Avg latency: ${avgLatency.toFixed(2)}ms`);
  console.log(`Min latency: ${minLatency}ms`);
  console.log(`Max latency: ${maxLatency}ms`);
  console.log(`Out of order messages: ${stats.outOfOrder}`);
  console.log(`Errors: ${stats.errors}`);

  console.log('\n--- Channel Statistics ---');
  channelStats.forEach((cs) => {
    console.log(`${cs.channel}: Published=${cs.published}, Received=${cs.received}, ` + `Expected=${cs.expectedReceived}, Missed=${cs.missed}, Delivery Rate=${cs.deliveryRate}`);
  });

  console.log('================================================\n');

  // Overall test assessment
  const deliveryRate = (totalReceived / expectedReceived) * 100;
  if (deliveryRate > 99.5 && stats.outOfOrder === 0 && stats.errors === 0) {
    console.log('TEST RESULT: EXCELLENT - Broker demonstrated exceptional stability and performance');
  } else if (deliveryRate > 98 && stats.outOfOrder < 100 && stats.errors < 10) {
    console.log('TEST RESULT: GOOD - Broker performed well with minimal issues');
  } else if (deliveryRate > 95) {
    console.log('TEST RESULT: ACCEPTABLE - Broker functioned but showed some stability issues');
  } else {
    console.log('TEST RESULT: POOR - Broker exhibited significant reliability problems');
  }
}

// Run the test
runHardcoreTest().catch((err) => {
  console.error('Critical test error:', err);
  process.exit(1);
});
