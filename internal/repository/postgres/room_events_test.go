package postgres

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
	_ "github.com/lib/pq"
)

func TestRoomEventsAppendAndListOrdered(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	turn0 := 0
	cipher0 := "cipher-0"
	senderA := "agt_a"
	ev1, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:     roomID,
		EventType:  "message.created",
		Turn:       &turn0,
		SenderID:   &senderA,
		Ciphertext: &cipher0,
	})
	if err != nil {
		t.Fatalf("append event 1: %v", err)
	}
	ev2, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "turn.changed",
	})
	if err != nil {
		t.Fatalf("append event 2: %v", err)
	}
	turn1 := 1
	cipher1 := "cipher-1"
	senderB := "agt_b"
	ev3, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:     roomID,
		EventType:  "message.created",
		Turn:       &turn1,
		SenderID:   &senderB,
		Ciphertext: &cipher1,
	})
	if err != nil {
		t.Fatalf("append event 3: %v", err)
	}

	items, err := store.ListRoomEvents(ctx, repository.ListRoomEventsInput{
		RoomID: roomID,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("events len=%d want=3", len(items))
	}
	if !(items[0].ID == ev1.ID && items[1].ID == ev2.ID && items[2].ID == ev3.ID) {
		t.Fatalf("unexpected event order ids=%v,%v,%v", items[0].ID, items[1].ID, items[2].ID)
	}

	next, err := store.ListRoomEvents(ctx, repository.ListRoomEventsInput{
		RoomID:  roomID,
		SinceID: ev1.ID,
		Limit:   100,
	})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(next) != 2 {
		t.Fatalf("events since len=%d want=2", len(next))
	}
	if next[0].ID != ev2.ID || next[1].ID != ev3.ID {
		t.Fatalf("unexpected events since order: %v then %v", next[0].ID, next[1].ID)
	}
}

func TestRoomEventsHydrateCiphertextFromMessages(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	msg, err := store.AppendMessage(ctx, repository.AppendMessageInput{
		ID:         "msg_events_hydrated",
		RoomID:     roomID,
		SenderID:   "agt_a",
		Turn:       0,
		Ciphertext: "cipher-hydrated",
	})
	if err != nil {
		t.Fatalf("append message: %v", err)
	}

	turn := msg.Turn
	sender := msg.SenderID
	if _, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "message.created",
		MessageID: &msg.ID,
		Turn:      &turn,
		SenderID:  &sender,
	}); err != nil {
		t.Fatalf("append room event: %v", err)
	}

	items, err := store.ListRoomEvents(ctx, repository.ListRoomEventsInput{
		RoomID: roomID,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("list room events: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("events len=%d want=1", len(items))
	}
	if items[0].Ciphertext == nil || *items[0].Ciphertext != msg.Ciphertext {
		t.Fatalf("event ciphertext=%v want=%q", items[0].Ciphertext, msg.Ciphertext)
	}
}

func TestRoomEventsDeletedOnPurge(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	if _, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "room.state_changed",
	}); err != nil {
		t.Fatalf("append room event: %v", err)
	}

	if err := store.PurgeRoomContent(ctx, roomID, time.Now().UTC()); err != nil {
		t.Fatalf("purge room: %v", err)
	}

	items, err := store.ListRoomEvents(ctx, repository.ListRoomEventsInput{
		RoomID: roomID,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("list events after purge: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("events after purge len=%d want=0", len(items))
	}
}

func TestRoomEventWriteRollbackWithTx(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	seedAgentsForEvents(t, ctx, store)

	ttl := time.Now().UTC().Add(30 * time.Minute)
	err := store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		_, txErr := tx.CreateRoom(ctx, repository.CreateRoomInput{
			ID:            "room_tx_rollback",
			AgentAID:      "agt_a",
			AgentBID:      "agt_b",
			State:         domain.RoomStateOpen,
			TurnIndex:     0,
			MaxTurns:      8,
			TTLAt:         ttl,
			HumanCodeHash: "hc_hash",
		})
		if txErr != nil {
			return txErr
		}
		_, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    "room_missing_fk",
			EventType: "room.state_changed",
		})
		return txErr
	})
	if err == nil {
		t.Fatal("expected tx failure from FK violation")
	}

	_, err = store.GetRoom(ctx, "room_tx_rollback")
	if err == nil {
		t.Fatal("expected room creation to rollback")
	}
	if err != repository.ErrNotFound {
		t.Fatalf("get room after rollback err=%v want=%v", err, repository.ErrNotFound)
	}
}

func openTestPostgresDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN (or POSTGRES_DSN) to run postgres room event tests")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("ping db: %v", err)
	}
	return db
}

func applyStoreMigrationsForTest(t *testing.T, db *sql.DB) {
	t.Helper()

	migDir := storeMigrationsDir(t)

	down, err := os.ReadFile(filepath.Join(migDir, "000001_init.down.sql"))
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	up1, err := os.ReadFile(filepath.Join(migDir, "000001_init.up.sql"))
	if err != nil {
		t.Fatalf("read init up migration: %v", err)
	}
	up2, err := os.ReadFile(filepath.Join(migDir, "000002_room_context_state.up.sql"))
	if err != nil {
		t.Fatalf("read room context up migration: %v", err)
	}
	up3, err := os.ReadFile(filepath.Join(migDir, "000003_room_events.up.sql"))
	if err != nil {
		t.Fatalf("read room events up migration: %v", err)
	}
	up4, err := os.ReadFile(filepath.Join(migDir, "000004_api_request_logs.up.sql"))
	if err != nil {
		t.Fatalf("read api request logs up migration: %v", err)
	}
	up5, err := os.ReadFile(filepath.Join(migDir, "000005_owner_first_listing_flow.up.sql"))
	if err != nil {
		t.Fatalf("read owner-first listing flow up migration: %v", err)
	}
	up6, err := os.ReadFile(filepath.Join(migDir, "000006_webhook_foundation.up.sql"))
	if err != nil {
		t.Fatalf("read webhook foundation up migration: %v", err)
	}
	up7, err := os.ReadFile(filepath.Join(migDir, "000007_webhook_endpoint_delete_cascade.up.sql"))
	if err != nil {
		t.Fatalf("read webhook endpoint delete cascade up migration: %v", err)
	}
	up8, err := os.ReadFile(filepath.Join(migDir, "000008_agent_stream_deliveries.up.sql"))
	if err != nil {
		t.Fatalf("read agent stream deliveries up migration: %v", err)
	}
	up9, err := os.ReadFile(filepath.Join(migDir, "000009_human_code_ttl.up.sql"))
	if err != nil {
		t.Fatalf("read human code ttl up migration: %v", err)
	}
	up10, err := os.ReadFile(filepath.Join(migDir, "000010_stream_coordination.up.sql"))
	if err != nil {
		t.Fatalf("read stream coordination up migration: %v", err)
	}
	up11, err := os.ReadFile(filepath.Join(migDir, "000011_human_code_ttl_backfill.up.sql"))
	if err != nil {
		t.Fatalf("read human code ttl backfill up migration: %v", err)
	}
	up12, err := os.ReadFile(filepath.Join(migDir, "000012_agent_policy_state.up.sql"))
	if err != nil {
		t.Fatalf("read agent policy state up migration: %v", err)
	}
	up13, err := os.ReadFile(filepath.Join(migDir, "000013_room_message_key.up.sql"))
	if err != nil {
		t.Fatalf("read room message key up migration: %v", err)
	}
	up14, err := os.ReadFile(filepath.Join(migDir, "000014_room_topic.up.sql"))
	if err != nil {
		t.Fatalf("read room topic up migration: %v", err)
	}
	up15, err := os.ReadFile(filepath.Join(migDir, "000015_api_request_logs_route_name.up.sql"))
	if err != nil {
		t.Fatalf("read api request logs route name up migration: %v", err)
	}

	if _, err := db.Exec(string(down)); err != nil {
		t.Fatalf("exec down migration: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_stream_deliveries`); err != nil {
		t.Fatalf("cleanup agent stream deliveries: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_event_stream_open_events`); err != nil {
		t.Fatalf("cleanup room event stream open events: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_event_stream_leases`); err != nil {
		t.Fatalf("cleanup room event stream leases: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_policy_state`); err != nil {
		t.Fatalf("cleanup agent policy state: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_scoped_tokens`); err != nil {
		t.Fatalf("cleanup room scoped tokens: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS webhook_outbox`); err != nil {
		t.Fatalf("cleanup webhook outbox: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_webhook_endpoints`); err != nil {
		t.Fatalf("cleanup agent webhook endpoints: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS api_request_logs`); err != nil {
		t.Fatalf("cleanup api request logs: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_context_state`); err != nil {
		t.Fatalf("cleanup room context state: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_events`); err != nil {
		t.Fatalf("cleanup room events: %v", err)
	}
	if _, err := db.Exec(string(up1)); err != nil {
		t.Fatalf("exec init up migration: %v", err)
	}
	if _, err := db.Exec(string(up2)); err != nil {
		t.Fatalf("exec room context up migration: %v", err)
	}
	if _, err := db.Exec(string(up3)); err != nil {
		t.Fatalf("exec room events up migration: %v", err)
	}
	if _, err := db.Exec(string(up4)); err != nil {
		t.Fatalf("exec api request logs up migration: %v", err)
	}
	if _, err := db.Exec(string(up5)); err != nil {
		t.Fatalf("exec owner-first listing flow up migration: %v", err)
	}
	if _, err := db.Exec(string(up6)); err != nil {
		t.Fatalf("exec webhook foundation up migration: %v", err)
	}
	if _, err := db.Exec(string(up7)); err != nil {
		t.Fatalf("exec webhook endpoint delete cascade up migration: %v", err)
	}
	if _, err := db.Exec(string(up8)); err != nil {
		t.Fatalf("exec agent stream deliveries up migration: %v", err)
	}
	if _, err := db.Exec(string(up9)); err != nil {
		t.Fatalf("exec human code ttl up migration: %v", err)
	}
	if _, err := db.Exec(string(up10)); err != nil {
		t.Fatalf("exec stream coordination up migration: %v", err)
	}
	if _, err := db.Exec(string(up11)); err != nil {
		t.Fatalf("exec human code ttl backfill up migration: %v", err)
	}
	if _, err := db.Exec(string(up12)); err != nil {
		t.Fatalf("exec agent policy state up migration: %v", err)
	}
	if _, err := db.Exec(string(up13)); err != nil {
		t.Fatalf("exec room message key up migration: %v", err)
	}
	if _, err := db.Exec(string(up14)); err != nil {
		t.Fatalf("exec room topic up migration: %v", err)
	}
	if _, err := db.Exec(string(up15)); err != nil {
		t.Fatalf("exec api request logs route name up migration: %v", err)
	}
}

func storeMigrationsDir(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "migrations"))
}

func seedRoomForEvents(t *testing.T, ctx context.Context, store *Store) string {
	t.Helper()

	seedAgentsForEvents(t, ctx, store)

	room, err := store.CreateRoom(ctx, repository.CreateRoomInput{
		ID:            "room_events_test",
		AgentAID:      "agt_a",
		AgentBID:      "agt_b",
		State:         domain.RoomStateOpen,
		TurnIndex:     0,
		MaxTurns:      8,
		TTLAt:         time.Now().UTC().Add(30 * time.Minute),
		HumanCodeHash: "hc_hash",
	})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	return room.ID
}

func seedAgentsForEvents(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	if _, err := store.CreateAgent(ctx, repository.CreateAgentInput{
		ID:         "agt_a",
		Name:       "agent-a",
		APIKeyHash: "hash-a",
	}); err != nil {
		t.Fatalf("create agent a: %v", err)
	}
	if _, err := store.CreateAgent(ctx, repository.CreateAgentInput{
		ID:         "agt_b",
		Name:       "agent-b",
		APIKeyHash: "hash-b",
	}); err != nil {
		t.Fatalf("create agent b: %v", err)
	}
}
