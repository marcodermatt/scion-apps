# Hello FABRID

A simple application using SCION and FABRID, that sends packets from a client to a server
which replies back.

## Setup

1. Prepare scion repo (assuming dependencies and bazel-remote already installed):
    1. `git clone git@github.com:marcodermatt/scion.git`
    2. `make`
    3. `./scion.sh topology` (DRKey enabled for this branch)
    4. `./scion.sh start`
2. Prepare scion-apps repo:
   1. Add to `go.mod` and `_examples/go.mod`:`replace github.com/scionproto/scion => path-to-marcodermatt-scion`
   2. `make examples-hellofabrid`
3. Choose two ASes and look-up SCION daemon addresses in `gen/sciond_addresses.json`, or use the ones below
4. Server:
   1. `export SCION_DAEMON_ADDRESS=127.0.0.60:30255`
   2. `./bin/example-hellofabrid -listen 127.0.0.1:1234`
4. Client:
    1. `export SCION_DAEMON_ADDRESS=127.0.0.133:30255`
    2. `./bin/example-hellofabrid -remote 1-ff00:0:112,127.0.0.1:1234`

```
Usage of ./bin/example-hellofabrid:
  -count uint
        [Client] Number of messages to send (default 1)
  -listen value
        [Server] local IP:port to listen on
  -log.console string
        Console logging level: debug|info|error (default "info")
  -max-val-ratio uint
        [Server] Maximum allowed validation ratio (default 255)
  -remote string
        [Client] Remote (i.e. the server's) SCION Address (e.g. 17-ffaa:1:1,[127.0.0.1]:12345)
  -val-ratio uint
        [Client] Requested validation ratio

```

See comments in `_examples/hellofabrid.go` for comments on configuration and validation handling.