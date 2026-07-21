#!/bin/sh
set -e

# Start Chrome's headless-shell in the background, listening only on
# localhost (nothing outside this container needs to reach it directly).
/headless-shell/headless-shell \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port=9222 \
  --headless \
  --disable-gpu \
  --no-sandbox \
  --disable-dev-shm-usage \
  --disable-extensions \
  --disable-background-networking \
  --disable-default-apps \
  --disable-sync \
  --mute-audio \
  --no-first-run \
  --metrics-recording-only \
  &

# Give it a moment to start listening before our app tries to connect.
sleep 1

# Run our Go app in the foreground — this is the process Render tracks.
exec ./app
