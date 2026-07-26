#!/usr/bin/env python3
"""
Counting test for the ShadowBroker, Python version.
Similar to testcounting.js but using a pooling approach.
"""

import time
import random
import threading
from typing import List, Dict
from dataclasses import dataclass

from shadow_client import ShadowClientPy

# Configuration
TOTAL_NUMBERS = 120
POOL_SIZE = 3  # Number of listeners in the pool


@dataclass
class Listener:
    """Represents a listener with its own sum tracker"""

    name: str
    client: ShadowClientPy
    sum: int = 0
    done: bool = False


def setup_listener_pool(count: int) -> List[Listener]:
    """Create a pool of listeners for the counting test"""
    pool = []

    for i in range(count):
        try:
            client = ShadowClientPy()
            listener = Listener(name=f"Listener {i+1}", client=client)
            pool.append(listener)
            print(f"Created {listener.name}")
        except Exception as e:
            print(f"Error creating listener {i+1}: {e}")
            # Try once more
            time.sleep(0.5)
            client = ShadowClientPy()
            listener = Listener(name=f"Listener {i+1}", client=client)
            pool.append(listener)
            print(f"Created {listener.name} on retry")

    return pool


def subscribe_listener(listener: Listener) -> None:
    """Subscribe a listener to the counting channel"""

    def on_message(message: str) -> None:
        num = int(message)

        if num < 0:
            print(f"{listener.name} finished: Final sum = {listener.sum}")
            listener.done = True
            return

        listener.sum += num
        print(f"{listener.name} received: {num}, Current sum: {listener.sum}")

    try:
        listener.client.subscribe("counting_120", on_message=on_message)
        print(f"{listener.name} started and listening to 'counting_120'")
    except Exception as e:
        print(f"Error subscribing {listener.name}: {e}")
        # Try once more
        time.sleep(0.5)
        try:
            listener.client.subscribe("counting_120", on_message=on_message)
            print(f"{listener.name} subscribed on retry")
        except Exception as e2:
            print(f"Failed to subscribe {listener.name} after retry: {e2}")


def run_pooled_test() -> None:
    """Run the counting test using a pool of listeners"""
    print("Starting counting test with pooled listeners...")

    # Create publisher client
    try:
        publisher = ShadowClientPy()
    except Exception as e:
        print(f"Error creating publisher: {e}")
        time.sleep(0.5)
        publisher = ShadowClientPy()

    # Create and setup listener pool
    listener_pool = setup_listener_pool(POOL_SIZE)

    # Start all listeners
    for listener in listener_pool:
        subscribe_listener(listener)

    # Give listeners time to connect
    print("Waiting for listeners to connect...")
    time.sleep(1)

    # Start publishing numbers
    print("Starting to publish numbers...")
    sum_total = 0

    for i in range(TOTAL_NUMBERS):
        # Generate random number between 1 and 100
        number = random.randint(1, 100)
        sum_total += number

        # Publish the number
        try:
            publisher.publish("counting_120", str(number))
            print(f"Published: {number}, Running sum: {sum_total}")
        except Exception as e:
            print(f"Error publishing number {number}: {e}")
            time.sleep(0.2)
            try:
                publisher.publish("counting_120", str(number))
                print(f"Published: {number} on retry")
            except Exception as e2:
                print(f"Failed to publish {number} after retry: {e2}")

        # Random delay between 0.01 and 0.2 seconds (10-200ms)
        delay = random.uniform(0.01, 0.2)
        time.sleep(delay)

    # Send final negative number to signal end
    try:
        publisher.publish("counting_120", "-1")
        print(f"Test complete. Final sum: {sum_total}")
    except Exception as e:
        print(f"Error publishing termination signal: {e}")
        publisher.publish("counting_120", "-1")

    # Wait for all listeners to finish
    print("Waiting for all listeners to finish...")
    wait_start = time.time()
    timeout = 5  # seconds

    while time.time() - wait_start < timeout:
        if all(listener.done for listener in listener_pool):
            break
        time.sleep(0.1)

    # Verify results
    all_correct = True
    for listener in listener_pool:
        if listener.sum != sum_total:
            print(
                f"{listener.name} has incorrect sum: {listener.sum} (expected {sum_total})"
            )
            all_correct = False

    if all_correct:
        print("SUCCESS: All listeners received the correct sum!")
    else:
        print("WARNING: Some listeners have incorrect sums!")


if __name__ == "__main__":
    try:
        run_pooled_test()
    except Exception as e:
        print(f"Critical test error: {e}")
        import sys

        sys.exit(1)
