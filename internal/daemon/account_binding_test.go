package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/upload"
)

func TestRestartWithNewAuthorizationDeadLettersOldQueueWithoutProviderIO(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.FS.WriteFile(ctx, "/demo/old-account.txt", []byte("retained"), false); err != nil {
		d.Close()
		t.Fatal(err)
	}
	rows, err := d.Journal.Pending(ctx)
	if err != nil || len(rows) != 1 || rows[0].AccountBinding == "" {
		d.Close()
		t.Fatalf("queue: %+v %v", rows, err)
	}
	id := rows[0].ID
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	remote := cfg.Remotes["demo"]
	remote.AccountBinding = config.NewAccountBinding()
	cfg.Remotes["demo"] = remote
	d, err = Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	before := d.CallStats["demo"].Total()
	if _, err := d.Uploader.Flush(ctx); !errors.Is(err, upload.ErrDeadLetters) {
		t.Fatalf("flush=%v", err)
	}
	if got := d.CallStats["demo"].Total(); got != before {
		t.Fatalf("old account task made %d provider calls", got-before)
	}
	row, err := d.Journal.Get(ctx, id)
	if err != nil || row.State != journal.StateDead || row.LastError == "" {
		t.Fatalf("row: %+v %v", row, err)
	}
}
