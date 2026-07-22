#!/bin/sh
set -e

echo "[start.sh] launching headless-shell..."

# Start Chrome's headless-shell in the background, listening only on
# localhost (nothing outside this container needs to reach it directly).
/headless-shell/headless-shell \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port=9222 \
  --headless=new \
  --disable-gpu \
  --no-sandbox \
  --disable-setuid-sandbox \
  --disable-dev-shm-usage \
  --disable-extensions \
  --disable-background-networking \
  --disable-default-apps \
  --disable-sync \
  --mute-audio \
  --no-first-run \
  --metrics-recording-only &
CHROME_PID=$!

# Actively wait for it to start answering, instead of a fixed sleep — up to
# ~15 seconds. If it dies immediately (crash), report that clearly instead
# of silently continuing to start the Go app.
i=0
while [ $i -lt 30 ]; do
  if ! kill -0 "$CHROME_PID" 2>/dev/null; then
    echo "[start.sh] ERROR: headless-shell process exited before it started listening."
    wait "$CHROME_PID"
    exit 1
  fi
  if curl -s -o /dev/null "http://127.0.0.1:9222/json/version"; then
    echo "[start.sh] headless-shell is up after $((i * 500))ms."
    break
  fi
  i=$((i + 1))
  sleep 0.5
done

if [ $i -eq 30 ]; then
  echo "[start.sh] WARNING: headless-shell did not answer after 15s, starting app anyway."
fi

echo "[start.sh] starting Go app..."
exec ./app
