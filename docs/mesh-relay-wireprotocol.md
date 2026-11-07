# SIERRA Mesh Relay — Wire Protocol v1

The spec lives in **[sierragridteam/sierra-meshrelay](https://github.com/sierragridteam/sierra-meshrelay)**: see [PROTOCOL.md](https://github.com/sierragridteam/sierra-meshrelay/blob/main/PROTOCOL.md).

That repo holds the protocol, the relay and the bridge together. Its Go types (package `meshproto`) are tested against the spec's examples, and its end-to-end tests run the real relay against the real bridge. sierra-data deploys the relay (`cmd/mesh-relay`) by importing it.
