# log-generator.py
# UPDATED for the exact LogEvent format your sidecar-agent-log-santos expects
# The generator ONLY outputs the RAW inner JSON that the agent parses.
# The agent then adds:
#   • service_id
#   • @timestamp (or overrides if you include "@timestamp")
#   • Docker metadata inside "attrs" (container_id, container_name, image, compose_project, stream)
#
# This produces logs that after processing look EXACTLY like the example you pasted:
#   - Some logs with NO "attrs" key
#   - Some logs with rich custom "attrs" + Docker metadata injected by the agent
#   - Flexible timestamps, trace/span, levels (DEBUG/INFO/WARN/ERROR)

import json
import os
import random
import time
import uuid
from datetime import datetime, timezone

def generate_log_event() -> dict:
    level = random.choices(
        ["DEBUG", "INFO", "WARN", "ERROR"],
        weights=[10, 60, 20, 10],
        k=1
    )[0]

    # Messages similar to your example
    messages = {
        "DEBUG": [
            "Checking credentials",
            "Validating token",
            "Parsing request body",
        ],
        "INFO": [
            "User authenticated successfully",
            "Payment processed",
            "Order created",
            "Background job completed",
        ],
        "WARN": [
            "Payment gateway slow response",
            "High latency detected",
            "Rate limit approaching",
        ],
        "ERROR": [
            "Database connection failed",
            "Authentication failed",
            "External service timeout",
        ],
    }

    message = random.choice(messages.get(level, messages["INFO"]))

    # Optional trace/span (80% of logs)
    trace_id = str(uuid.uuid4())[:12] if random.random() < 0.8 else None
    span_id = str(uuid.uuid4())[:8] if random.random() < 0.75 and trace_id else None

    # Optional custom attrs (sometimes omitted to match your example)
    attrs = None
    if random.random() < 0.65:  # 65% of logs have custom attrs
        attrs = {}
        if level == "WARN" and "Payment gateway" in message:
            attrs["gateway"] = random.choice(["stripe", "paypal", "adyen"])
            attrs["latency_ms"] = random.randint(800, 4500)
        elif level == "INFO":
            attrs["user_id"] = random.randint(10000, 99999)
            attrs["endpoint"] = random.choice(["/api/v1/orders", "/api/v1/users", "/health"])
        elif level == "ERROR":
            attrs["error_code"] = random.choice(["500", "429", "TIMEOUT", "DB_ERROR"])
        if random.random() < 0.4:
            attrs["request_id"] = f"req-{uuid.uuid4().hex[:8]}"

    event = {
        "level": level,
        "message": message,
    }
    if trace_id:
        event["trace_id"] = trace_id
    if span_id:
        event["span_id"] = span_id

    # You can include "@timestamp" if you want to control the exact time
    # (agent will use it instead of Docker timestamp)
    if random.random() < 0.3:
        event["@timestamp"] = datetime.now(timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')

    if attrs:
        event["attrs"] = attrs

    return event


def main():
    rate = int(os.getenv("LOG_RATE", "1500"))           # logs per second – tune for load test
    duration = int(os.getenv("DURATION_SECONDS", "0"))  # 0 = run forever
    burst_every = int(os.getenv("BURST_EVERY", "40"))
    burst_size = int(os.getenv("BURST_SIZE", "250"))

    print(f"🚀 Log generator started | rate={rate} logs/sec | burst every {burst_every} logs (+{burst_size})")
    if duration > 0:
        print(f"⏱️  Will stop after {duration} seconds")

    start_time = time.time()
    total_sent = 0
    next_burst = burst_every

    while True:
        if duration > 0 and (time.time() - start_time) >= duration:
            print(f"✅ Finished. Total logs sent: {total_sent}")
            break

        event = generate_log_event()
        print(json.dumps(event), flush=True)
        total_sent += 1

        # Burst mode (realistic traffic spikes)
        if total_sent >= next_burst:
            print(f"💥 BURST! Sending {burst_size} extra logs...")
            for _ in range(burst_size):
                event = generate_log_event()
                print(json.dumps(event), flush=True)
                total_sent += 1
            next_burst = total_sent + burst_every

        # Precise rate control
        elapsed = time.time() - start_time
        target = int(elapsed * rate)
        if total_sent < target:
            continue
        time.sleep(max(0.001, 1.0 / rate))

    print(f"🏁 Generator stopped. Total logs: {total_sent}")


if __name__ == "__main__":
    main()
