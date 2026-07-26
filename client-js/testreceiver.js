import { ShadowClient } from './shadowClient.js';

const client = new ShadowClient();

client.subscribe('demo', (message) => {
  console.log('JS => Incoming message on "demo":', message);
});

client.subscribe('demo2', (message) => {
  console.log('JS => Incoming message on "demo2":', message);
});

// infinite loop
while (true) {
  await new Promise((resolve) => setTimeout(resolve, 1000));
}
