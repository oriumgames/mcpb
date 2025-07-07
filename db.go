package mcpb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/cockroachdb/pebble"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// DB implements a world provider for the Minecraft world format, which
// is based on a pebble database.
type DB struct {
	conf Config
	pdb  *pebble.DB
	dir  string
	set  *world.Settings
}

// Open creates a new provider reading and writing from/to files under the path
// passed using default options. If a world is present at the path, Open will
// parse its data and initialise the world with it. If the data cannot be
// parsed, an error is returned.
func Open(dir string) (*DB, error) {
	var conf Config
	return conf.Open(dir)
}

// Settings returns the world.Settings of the world loaded by the DB.
func (db *DB) Settings() *world.Settings {
	return db.set
}

// SaveSettings saves the world.Settings passed to the level.dat.
func (db *DB) SaveSettings(s *world.Settings) {
	// nop
}

// LoadPlayerSpawnPosition loads the players spawn position stored in the level.dat from their UUID.
func (db *DB) LoadPlayerSpawnPosition(id uuid.UUID) (pos cube.Pos, exists bool, err error) {
	return cube.Pos{}, false, nil
}

// SavePlayerSpawnPosition saves the player spawn position passed to the levelDB database.
func (db *DB) SavePlayerSpawnPosition(id uuid.UUID, pos cube.Pos) error {
	return nil
}

// LoadColumn reads a world.Column from the DB at a position and dimension in
// the DB. If no column at that position exists, errors.Is(err,
// leveldb.ErrNotFound) equals true.
func (db *DB) LoadColumn(pos world.ChunkPos, dim world.Dimension) (*chunk.Column, error) {
	k := dbKey{pos: pos, dim: dim}
	col, err := db.column(k)
	if err != nil {
		return nil, fmt.Errorf("load column %v (%v): %w", pos, dim, err)
	}
	return col, nil
}

const chunkVersion = 41

func (db *DB) column(k dbKey) (*chunk.Column, error) {
	var cdata chunk.SerialisedData
	col := new(chunk.Column)

	ver, err := db.version(k)
	if err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if ver != chunkVersion {
		db.conf.Log.Debug("column: unsupported chunk version, trying to load anyway", "X", k.pos[0], "Z", k.pos[1], "dimension", fmt.Sprint(k.dim), "ver", ver)
	}
	cdata.Biomes, err = db.biomes(k)
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		// Some chunks still use 2D chunk data and might not have this field, in
		// which case we can just move on.
		return nil, fmt.Errorf("read biomes: %w", err)
	}
	cdata.SubChunks, err = db.subChunks(k)
	if err != nil {
		return nil, fmt.Errorf("read sub chunks: %w", err)
	}
	col.Chunk, err = chunk.DiskDecode(cdata, k.dim.Range())
	if err != nil {
		return nil, fmt.Errorf("decode chunk data: %w", err)
	}
	col.Entities, err = db.entities(k)
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		// Not all chunks need to have entities, so an ErrNotFound is fine here.
		return nil, fmt.Errorf("read entities: %w", err)
	}
	col.BlockEntities, err = db.blockEntities(k)
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		// Same as with entities, an ErrNotFound is fine here.
		return nil, fmt.Errorf("read block entities: %w", err)
	}
	col.ScheduledBlocks, col.Tick, err = db.scheduledUpdates(k)
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		return nil, fmt.Errorf("read scheduled updates: %w", err)
	}
	return col, nil
}

func (db *DB) wrappedGet(key []byte) ([]byte, io.Closer, error) {
	val, closer, err := db.pdb.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, nil, leveldb.ErrNotFound
	}
	return val, closer, err
}

func (db *DB) version(k dbKey) (byte, error) {
	p, closer, err := db.wrappedGet(k.Sum(keyVersion))
	if err != nil {
		return 0, err
	}
	defer closer.Close()

	if n := len(p); n != 1 {
		return 0, fmt.Errorf("expected 1 version byte, got %v", n)
	}
	return p[0], nil
}

func (db *DB) biomes(k dbKey) ([]byte, error) {
	biomes, closer, err := db.wrappedGet(k.Sum(key3DData))
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	// The first 512 bytes is a heightmap (16*16 int16s), the biomes follow. We
	// calculate a heightmap on startup so the heightmap can be discarded.
	if n := len(biomes); n <= 512 {
		return nil, fmt.Errorf("expected at least 513 bytes for 3D data, got %v", n)
	}
	return biomes[512:], nil
}

func (db *DB) subChunks(k dbKey) ([][]byte, error) {
	r := k.dim.Range()
	sub := make([][]byte, (r.Height()>>4)+1)

	for i := range sub {
		y := uint8(i + (r[0] >> 4))
		val, closer, err := db.wrappedGet(k.Sum(keySubChunkData, y))
		if errors.Is(err, pebble.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("sub chunk %v: %w", int8(i), err)
		}

		sub[i] = append([]byte(nil), val...)
		closer.Close()
	}
	return sub, nil
}

func (db *DB) entities(k dbKey) ([]chunk.Entity, error) {
	// https://learn.microsoft.com/en-us/minecraft/creator/documents/actorstorage
	ids, closer, err := db.wrappedGet(append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...))
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	entities := make([]chunk.Entity, 0, len(ids)/8)
	for i := 0; i < len(ids); i += 8 {
		id := int64(binary.LittleEndian.Uint64(ids[i : i+8]))

		data, closer, err := db.wrappedGet(entityIndex(id))
		if err != nil {
			db.conf.Log.Error("read entity: "+err.Error(), "ID", id)
			return nil, err
		}

		func() {
			defer closer.Close()

			ent := chunk.Entity{ID: id, Data: make(map[string]any)}
			if err = nbt.UnmarshalEncoding(data, &ent.Data, nbt.LittleEndian); err != nil {
				db.conf.Log.Error("decode entity nbt: "+err.Error(), "ID", id)
			}
			entities = append(entities, ent)
		}()
	}
	return entities, nil
}

func (db *DB) blockEntities(k dbKey) ([]chunk.BlockEntity, error) {
	var blockEntities []chunk.BlockEntity

	data, closer, err := db.wrappedGet(k.Sum(keyBlockEntities))
	if err != nil {
		return blockEntities, err
	}
	defer closer.Close()

	buf := bytes.NewBuffer(data)
	dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)

	for buf.Len() != 0 {
		be := chunk.BlockEntity{Data: make(map[string]any)}
		if err := dec.Decode(&be.Data); err != nil {
			return blockEntities, fmt.Errorf("decode nbt: %w", err)
		}
		be.Pos = blockPosFromNBT(be.Data)
		blockEntities = append(blockEntities, be)
	}
	return blockEntities, nil
}

func (db *DB) scheduledUpdates(k dbKey) ([]chunk.ScheduledBlockUpdate, int64, error) {
	data, closer, err := db.wrappedGet(k.Sum(keyPendingScheduledTicks))
	if err != nil {
		return nil, 0, err
	}
	defer closer.Close()

	var m scheduledUpdates
	if err := nbt.UnmarshalEncoding(data, &m, nbt.LittleEndian); err != nil {
		return nil, 0, fmt.Errorf("read nbt: %s", err.Error())
	}
	updates := make([]chunk.ScheduledBlockUpdate, len(m.TickList))
	for i, tick := range m.TickList {
		t, _ := tick["time"].(int64)
		bl, _ := tick["blockState"].(map[string]any)
		block, err := chunk.BlockPaletteEncoding.DecodeBlockState(bl)
		if err != nil {
			db.conf.Log.Error("read scheduled updates: decode block state: " + err.Error())
			continue
		}
		updates[i] = chunk.ScheduledBlockUpdate{Pos: blockPosFromNBT(tick), Block: block, Tick: t}
	}
	return updates, int64(m.CurrentTick), nil
}

// StoreColumn stores a world.Column at a position and dimension in the DB. An
// error is returned if storing was unsuccessful.
func (db *DB) StoreColumn(pos world.ChunkPos, dim world.Dimension, col *chunk.Column) error {
	k := dbKey{pos: pos, dim: dim}
	if err := db.storeColumn(k, col); err != nil {
		return fmt.Errorf("store column %v (%v): %w", pos, dim, err)
	}
	return nil
}

func (db *DB) storeColumn(k dbKey, col *chunk.Column) error {
	data := chunk.Encode(col.Chunk, chunk.DiskEncoding)
	batch := db.pdb.NewBatch()
	defer batch.Close()

	db.storeVersion(batch, k, chunkVersion)
	db.storeBiomes(batch, k, data.Biomes)
	db.storeSubChunks(batch, k, data.SubChunks, col.Chunk.Range())
	db.storeFinalisation(batch, k, finalisationPopulated)
	db.storeEntities(batch, k, col.Entities)
	db.storeBlockEntities(batch, k, col.BlockEntities)
	db.storeScheduledUpdates(batch, k, col.Tick, col.ScheduledBlocks)

	return batch.Commit(pebble.Sync)
}

func (db *DB) storeVersion(batch *pebble.Batch, k dbKey, ver uint8) {
	batch.Set(k.Sum(keyVersion), []byte{ver}, nil)
}

var emptyHeightmap = make([]byte, 512)

func (db *DB) storeBiomes(batch *pebble.Batch, k dbKey, biomes []byte) {
	batch.Set(k.Sum(key3DData), append(emptyHeightmap, biomes...), nil)
}

func (db *DB) storeSubChunks(batch *pebble.Batch, k dbKey, subChunks [][]byte, r cube.Range) {
	for i, sub := range subChunks {
		batch.Set(k.Sum(keySubChunkData, byte(i+(r[0]>>4))), sub, nil)
	}
}

func (db *DB) storeFinalisation(batch *pebble.Batch, k dbKey, finalisation uint32) {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, finalisation)
	batch.Set(k.Sum(keyFinalisation), p, nil)
}

func (db *DB) storeEntities(batch *pebble.Batch, k dbKey, entities []chunk.Entity) {
	idsKey := append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...)

	// load the ids of the previous entities
	var previousIDs []int64
	digpPrev, closer, err := db.wrappedGet(idsKey)
	if err != nil && !errors.Is(err, pebble.ErrNotFound) {
		db.conf.Log.Error("store entities: read chunk entity IDs: " + err.Error())
	}
	if err == nil {
		defer closer.Close()

		for i := 0; i < len(digpPrev); i += 8 {
			previousIDs = append(previousIDs, int64(binary.LittleEndian.Uint64(digpPrev[i:])))
		}
	}

	newIDs := make([]int64, 0, len(entities))
	for _, e := range entities {
		e.Data["UniqueID"] = e.ID
		b, err := nbt.MarshalEncoding(e.Data, nbt.LittleEndian)
		if err != nil {
			db.conf.Log.Error("store entities: encode NBT: " + err.Error())
			continue
		}
		batch.Set(entityIndex(e.ID), b, nil)
		newIDs = append(newIDs, e.ID)
	}

	// Remove entities that are no longer referenced.
	for _, uniqueID := range previousIDs {
		if !slices.Contains(newIDs, uniqueID) {
			batch.Delete(entityIndex(uniqueID), nil)
		}
	}
	if len(entities) == 0 {
		batch.Delete(idsKey, nil)
	} else {
		// Save the index of entities in the chunk.
		ids := make([]byte, 0, 8*len(newIDs))
		for _, uniqueID := range newIDs {
			ids = binary.LittleEndian.AppendUint64(ids, uint64(uniqueID))
		}
		batch.Set(idsKey, ids, nil)
	}
}

func entityIndex(id int64) []byte {
	return binary.LittleEndian.AppendUint64([]byte(keyEntity), uint64(id))
}

func (db *DB) storeBlockEntities(batch *pebble.Batch, k dbKey, blockEntities []chunk.BlockEntity) {
	if len(blockEntities) == 0 {
		batch.Delete(k.Sum(keyBlockEntities), nil)
		return
	}

	buf := bytes.NewBuffer(nil)
	enc := nbt.NewEncoderWithEncoding(buf, nbt.LittleEndian)
	for _, b := range blockEntities {
		b.Data["x"], b.Data["y"], b.Data["z"] = int32(b.Pos[0]), int32(b.Pos[1]), int32(b.Pos[2])
		if err := enc.Encode(b.Data); err != nil {
			db.conf.Log.Error("store block entities: encode nbt: " + err.Error())
		}
	}
	batch.Set(k.Sum(keyBlockEntities), buf.Bytes(), nil)
}

func (db *DB) storeScheduledUpdates(batch *pebble.Batch, k dbKey, tick int64, updates []chunk.ScheduledBlockUpdate) {
	if len(updates) == 0 {
		batch.Delete(k.Sum(keyPendingScheduledTicks), nil)
		return
	}
	list := make([]map[string]any, len(updates))
	for i, update := range updates {
		list[i] = map[string]any{
			"x": int32(update.Pos[0]), "y": int32(update.Pos[1]), "z": int32(update.Pos[2]),
			"time": update.Tick, "blockState": chunk.BlockPaletteEncoding.EncodeBlockState(update.Block),
		}
	}
	b, err := nbt.MarshalEncoding(scheduledUpdates{CurrentTick: int32(tick), TickList: list}, nbt.LittleEndian)
	if err != nil {
		db.conf.Log.Error("store scheduled updates: encode nbt: " + err.Error())
		return
	}
	batch.Set(k.Sum(keyPendingScheduledTicks), b, nil)
}

type scheduledUpdates struct {
	CurrentTick int32            `nbt:"currentTick"`
	TickList    []map[string]any `nbt:"tickList"`
}

// NewColumnIterator returns a ColumnIterator that may be used to iterate over all
// position/chunk pairs in a database.
// An IteratorRange r may be passed to specify limits in terms of what chunks
// should be read. r may be set to nil to read all chunks from the DB.
func (db *DB) NewColumnIterator(r *IteratorRange) (*ColumnIterator, error) {
	if r == nil {
		r = &IteratorRange{}
	}
	return newColumnIterator(db, r)
}

// Close closes the provider, saving any file that might need to be saved, such as the level.dat.
func (db *DB) Close() error {
	return db.pdb.Close()
}

// dbKey holds a position and dimension.
type dbKey struct {
	pos world.ChunkPos
	dim world.Dimension
}

// Sum converts k to its []byte representation and appends p.
func (k dbKey) Sum(p ...byte) []byte {
	return append(index(k.pos, k.dim), p...)
}

// index returns a byte buffer holding the written index of the chunk position passed. If the dimension passed
// is not world.Overworld, the length of the index returned is 12. It is 8 otherwise.
func index(position world.ChunkPos, d world.Dimension) []byte {
	dim, _ := world.DimensionID(d)
	x, z := uint32(position[0]), uint32(position[1])
	b := make([]byte, 12)

	binary.LittleEndian.PutUint32(b, x)
	binary.LittleEndian.PutUint32(b[4:], z)
	if dim == 0 {
		return b[:8]
	}
	binary.LittleEndian.PutUint32(b[8:], uint32(dim))
	return b
}

// blockPosFromNBT returns a position from the X, Y and Z components stored in the NBT data map passed. The
// map is assumed to have an 'x', 'y' and 'z' key.
func blockPosFromNBT(data map[string]any) cube.Pos {
	x, _ := data["x"].(int32)
	y, _ := data["y"].(int32)
	z, _ := data["z"].(int32)
	return cube.Pos{int(x), int(y), int(z)}
}
