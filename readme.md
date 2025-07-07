# mcpb

a dragonfly world provider that stores level data using [pebble](https://github.com/cockroachdb/pebble)

(not intended for persistent storage)

based on [dragonfly/world/mcdb](https://github.com/df-mc/dragonfly/tree/master/server/world/mcdb)

## example usage
```go
conf := server.DefaultConfig()
conf.World.SaveData = false

cfg, _ := conf.Config(slog.Default())

db, _ := mcpb.Open(usrConf.World.Folder)
cfg.WorldProvider = db

srv := cfg.New()
srv.CloseOnProgramEnd()
srv.Listen()

for range srv.Accept() {}
```
