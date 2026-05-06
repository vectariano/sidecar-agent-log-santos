#!/usr/bin/env bash
set -euo pipefail

export LOG_RATE=100
export DURATION_SECONDS=3600
export BURST_EVERY=1000000
export BURST_SIZE=0

docker compose up -d --build

echo "Ramp-up to 6 users in 5 minutes"

docker compose up -d --scale generator=1 generator
sleep 60

docker compose up -d --scale generator=2 generator
sleep 60

docker compose up -d --scale generator=3 generator
sleep 60

docker compose up -d --scale generator=4 generator
sleep 60

docker compose up -d --scale generator=5 generator
sleep 60

docker compose up -d --scale generator=6 generator
sleep 60

docker compose up -d --scale generator=7 generator

users=7

while true; do
    next_users=$(( (users * 110 + 99) / 100 ))

    if [$"next_users" -le $"users" ]; then
        next_users=$((users + 1))
    fi

    users=$next_users

    docker compose up -d --scale generator=${users} generator

    sleep 10
done
# echo "Plateau: 15 minutes at ~600 logs/sec"
# sleep 900

# docker compose stop generator