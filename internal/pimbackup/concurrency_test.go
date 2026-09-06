package pimbackup

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

type concurrentObjectSource struct {
	active atomic.Int32
	max    atomic.Int32
}

func (*concurrentObjectSource) Collections(context.Context) ([]remoteCollection, error) {
	return nil, nil
}

func (*concurrentObjectSource) Objects(context.Context, remoteCollection) ([]remoteObject, string, error) {
	return nil, "", nil
}

func (*concurrentObjectSource) Close() {}

func (source *concurrentObjectSource) Get(context.Context, remoteObject) (io.ReadCloser, string, error) {
	active := source.active.Add(1)
	for {
		maximum := source.max.Load()
		if active <= maximum || source.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	source.active.Add(-1)
	return io.NopCloser(strings.NewReader("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Test\r\nEND:VCARD\r\n")), "text/vcard", nil
}

func TestFetchObjectsUsesBoundedConcurrency(t *testing.T) {
	ctx := context.Background()
	service, err := OpenService(ctx, config.Config{DataDir: t.TempDir(), Log: config.LogConfig{Level: "info", Format: "json"}}, ServiceOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.catalog.UpsertAccount(ctx, "test", "carddav"); err != nil {
		t.Fatal(err)
	}
	collection, err := service.catalog.EnsureCollection(ctx, model.Collection{AccountID: "test", Kind: "contact", Name: "People", RemoteID: "/people/"})
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]remoteObject, 12)
	for index := range objects {
		objects[index] = remoteObject{RemoteID: string(rune('a' + index))}
	}
	source := &concurrentObjectSource{}
	fetched, _, err := service.fetchObjects(ctx, source, collection, objects)
	if err != nil {
		t.Fatal(err)
	}
	if fetched != len(objects) {
		t.Fatalf("fetched = %d, want %d", fetched, len(objects))
	}
	if maximum := source.max.Load(); maximum < 2 || maximum > objectTransfers {
		t.Fatalf("maximum concurrent transfers = %d", maximum)
	}
}
