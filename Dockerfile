### Build stage: static Go binary (no libc dependency, works on any base image) ###
FROM golang:1.23-alpine AS build
WORKDIR /src

RUN apk add --no-cache git

COPY go.mod ./
COPY main.go ./

# Pinned chromedp version: "latest" (v0.16.x) requires Go >= 1.26, which isn't
# released yet, so we pin the last version known to work with Go 1.2x.
RUN go get github.com/chromedp/chromedp@v0.9.5 \
 && GOFLAGS= go mod tidy \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS= go build -tags netgo -ldflags '-s -w' -o /out/app .

### Runtime stage: chromedp/headless-shell — a purpose-built, much lighter ###
### Chrome build than a full desktop Chromium install (what caused the      ###
### earlier out-of-memory crash on Render's free plan).                    ###
### Pinned to an older Chrome version: chromedp v0.9.5's cdproto doesn't    ###
### understand newer Chrome's "cookiePartitionKey" CDP fields, which shows ###
### up as "could not unmarshal event" errors and connection instability.   ###
FROM chromedp/headless-shell:113.0.5672.64

# tini reaps zombie processes left behind by Chrome's subprocesses — the
# chromedp/headless-shell image's own docs recommend this when you add your
# own program on top of it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini curl ca-certificates \
 && update-ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/app ./app
COPY start.sh ./start.sh
RUN chmod +x ./start.sh

EXPOSE 8080
ENTRYPOINT ["tini", "--", "./start.sh"]
