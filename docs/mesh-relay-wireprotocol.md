# SIERRA Mesh Relay — Wire Protocol v1

The spec has moved to **[dpup/sierra-meshrelay-protocol](https://github.com/dpup/sierra-meshrelay-protocol)**: see [PROTOCOL.md](https://github.com/dpup/sierra-meshrelay-protocol/blob/main/PROTOCOL.md).

That repo is the only copy. It also holds the Go types (package `meshproto`) that the relay (`internal/meshrelay`) and the bridge ([dpup/sierra-meshbridge](https://github.com/dpup/sierra-meshbridge)) both import, so a protocol change is one pull request there plus a module bump here.
