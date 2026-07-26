import { ShadowClient } from './shadowClient.js';

async function runTest() {
  const client = new ShadowClient();
  const totalNumbers = 120;
  let sum = 0;

  // Set up 3 listeners
  setupListener(client, 'Listener 1');
  setupListener(client, 'Listener 2');
  setupListener(client, 'Listener 3');

  // Give listeners time to connect
  await new Promise((resolve) => setTimeout(resolve, 1000));

  console.log('Starting to publish numbers...');

  // Generate and send 120 random numbers
  for (let i = 0; i < totalNumbers; i++) {
    // Generate random number between 1 and 100
    const number = Math.floor(Math.random() * 100) + 1;
    sum += number;

    await client.publish('counting_120', number.toString());
    console.log(`Published: ${number}, Running sum: ${sum}`);

    // Random delay between 0.1 and 2 seconds
    const delay = Math.random() * 190 + 10;
    await new Promise((resolve) => setTimeout(resolve, delay));
  }

  // Send final negative number to signal end
  await client.publish('counting_120', '-1');
  console.log(`Test complete. Final sum: ${sum}`);
}

function setupListener(client, name) {
  let listenerSum = 0;

  client.subscribe('counting_120', (message) => {
    const num = parseInt(message, 10);

    if (num < 0) {
      console.log(`${name} finished: Final sum = ${listenerSum}`);
      return;
    }

    listenerSum += num;
    console.log(`${name} received: ${num}, Current sum: ${listenerSum}`);
  });

  console.log(`${name} started and listening to 'counting_120'`);
}

runTest().catch((err) => console.error('Test error:', err));
