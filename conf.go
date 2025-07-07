package mcpb

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/cockroachdb/pebble"
	"github.com/df-mc/dragonfly/server/world"
)

// Config holds the optional parameters of a DB.
type Config struct {
	// Log is the Logger that will be used to log errors and debug messages to.
	// If set to nil, Log is set to slog.Default().
	Log *slog.Logger
	// PDBOptions holds PebbleDB specific default options, such as the block size
	// or compression used in the database.
	PDBOptions    *pebble.Options
	WorldSettings *world.Settings
}

// Open creates a new DB reading and writing from/to files under the path
// passed. If a world is present at the path, Open will parse its data and
// initialise the world with it. If the data cannot be parsed, an error is
// returned.
func (conf Config) Open(dir string) (*DB, error) {
	if conf.Log == nil {
		conf.Log = slog.Default()
	}
	conf.Log = conf.Log.With("provider", "mcpb")
	if conf.PDBOptions == nil {
		conf.PDBOptions = new(pebble.Options)
	}
	if len(conf.PDBOptions.Levels) == 0 {
		conf.PDBOptions.Levels = make([]pebble.LevelOptions, 7)
		for i := range conf.PDBOptions.Levels {
			conf.PDBOptions.Levels[i].BlockSize = 16 * 1024 // 16 KiB
		}
	}
	_ = os.MkdirAll(dir, 0777)

	if conf.WorldSettings == nil {
		conf.WorldSettings = &world.Settings{
			Name:            "World",
			DefaultGameMode: world.GameModeSurvival,
			Difficulty:      world.DifficultyNormal,
			TimeCycle:       true,
			WeatherCycle:    true,
			TickRange:       6,
		}
	}

	db := &DB{conf: conf, dir: dir}
	db.set = conf.WorldSettings

	pdb, err := pebble.Open(dir, conf.PDBOptions)
	if err != nil {
		return nil, fmt.Errorf("open db: pebble: %w", err)
	}
	db.pdb = pdb

	return db, nil
}
