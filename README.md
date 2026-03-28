# live.cursor

A websocket server to interact with people and play games with the mouse.

# How it works

The client accesses the website, registers their name or any message, and begins interacting.

- The server will be responsible for identifying each connected client by assigning them a unique UUID
(This allows the server to identify the user responsible for each message or interaction).
- The client will send mouse movements every 100ms via WebSockets.

Thanks to this prior UUID identification, the server can distinguish between each pointer interacting with the server.

# Run
You can start the server using go or docker

Using Go, you only need to do the following:
```bash
ADDR=:8080 ORIGIN=http://localhost:8080 go run .
```

Using Docker, you need to use the cursor-live image found in the GitHub Container Registry `ghcr.io`:
```bash
docker run -dp 8080:8080 -e ORIGIN=http://localhost:8080 -e ADDR=:8080 ghcr.io/loadept/live.cursor
```

- loadept
