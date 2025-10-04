# rssgotransmission

[![Docker Hub](https://img.shields.io/docker/pulls/tolnaiz/rssgotransmission.svg)](https://hub.docker.com/r/tolnaiz/rssgotransmission)
[![Go Report Card](https://goreportcard.com/badge/github.com/tolnaiz/rssgotransmission)](https://goreportcard.com/report/github.com/tolnaiz/rssgotransmission)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A minimal Go service that polls RSS feeds for `.torrent` or `magnet:` links and automatically adds them to a [Transmission](https://transmissionbt.com/) instance via RPC.  
Built to be lightweight, reliable, and easy to run in Docker.

---

## ✨ Features
- Polls one or more RSS/Atom feeds on a schedule
- Supports:
  - `<enclosure>` with `.torrent`
  - `<link>` pointing to `.torrent`
  - `magnet:` URIs
- Deduplication via JSON state file
- Works well with private and public trackers
- Minimal memory/CPU usage
- Runs standalone binary or Docker container

---

## 🚀 Usage

### Docker (recommended)

```bash
docker run -d \
  --name rssgotransmission \
  -e FEEDS="https://yourfeed.com/rss.php" \
  -e TRANSMISSION_URL="http://transmission:9091/transmission/rpc" \
  -e TRANSMISSION_USER=user \
  -e TRANSMISSION_PASS=pass \
  -e DOWNLOAD_DIR=/downloads \
  -e INTERVAL=10m \
  -e STATE_PATH=/data/state.json \
  -e DEBUG=false \
  -v ./state:/data \
  tolnaiz/rssgotransmission:latest
