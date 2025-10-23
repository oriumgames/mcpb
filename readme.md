# mcpb
Pebble world provider for dragonfly

## Highlights
- Uses [Pebble](https://github.com/cockroachdb/pebble) as the underlying key-value store.
- Simple integration as a drop-in world provider.
- Not intended for long-term persistence.

## Install
```sh
go get github.com/oriumgames/mcpb
```

## Quick Start
```go
package main

import (
    "github.com/oriumgames/mcpb"
    "github.com/df-mc/dragonfly/server"
    "log/slog"
)

func main() {
    conf := server.DefaultConfig()
    conf.World.SaveData = false

    cfg, _ := conf.Config(slog.Default())
    db, _ := mcpb.Open(cfg.World.Folder)
    cfg.WorldProvider = db

    srv := cfg.New()
    srv.CloseOnProgramEnd()
    srv.Listen()

    for range srv.Accept() {}
}
```

## Acknowledgments
This work is based on [dragonfly/world/mcdb](https://github.com/df-mc/dragonfly/tree/master/server/world/mcdb).
