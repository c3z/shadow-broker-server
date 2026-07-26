import { ShadowClient } from './shadowClient.js';

const client = new ShadowClient();

const animals = ['dog', 'cat', 'bird', 'fish', 'snake', 'albatros'];
// infinite loop
while (true) {
  // generate random animal name and send it to the server
  const animal = animals[Math.floor(Math.random() * animals.length)];
  await client.publish('noti', JSON.stringify({ animal: animal }));
  // generate random number between 1 and 10
  const number = Math.floor(Math.random() * 10) + 1;
  await client.publish('demo2', 'number: ' + number);
  await new Promise((resolve) => setTimeout(resolve, 1000));
}
