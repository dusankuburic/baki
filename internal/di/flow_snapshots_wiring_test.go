package di

import (
	"context"
	"testing"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/parser"
)

const wiringSnapshotSrc = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://x''' Method: HTTPClient.Method.GET\n#EndRegion\n"

// savedSnapshots saves an edit of a stored flow through a service built with opts and counts the snapshots the
// backend then holds for it: the undo ring is in the database exactly when the deployment has a server store.
func savedSnapshots(t *testing.T, opts []service.FlowOption, backend *testutil.FakeBackend) int {
	t.Helper()
	ctx := context.Background()
	doc, err := parser.ParseText(wiringSnapshotSrc, "Main.txt", int64(len(wiringSnapshotSrc)))
	if err != nil {
		t.Fatal(err)
	}
	doc.ID, doc.OwnerID, doc.Source = "wiring-flow", "u1", wiringSnapshotSrc
	doc.RebuildIndexes()
	var store storageif.StorageBackend
	if backend != nil {
		store = backend
	}
	svc := service.NewFlowService(&testutil.CountingNotifier{}, nil, service.NewLocalDocumentProvider(), store, nil, nil, opts...)
	if _, err := svc.SaveSource(ctx, doc, wiringSnapshotSrc+"\n", ""); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	if backend == nil {
		return 0
	}
	got, err := backend.ListFlowSnapshots(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

// Cloud and local+db have the server store: their FlowService keeps the undo ring in it. The desktop has none and
// keeps the memory ring. Asked of the capabilities, not of the mode or a nil check on the backend alone.
func TestSnapshotStoreOptions_FollowTheServerStoreCapability(t *testing.T) {
	cloud, localDB, desktop := config.Default(), config.Default(), config.Default()
	cloud.Mode, cloud.Auth.Enabled, cloud.Storage.Backend = config.ModeCloud, true, config.StorageDatabase
	localDB.Mode, localDB.Storage.Backend = config.ModeLocal, config.StorageDatabase
	desktop.Mode, desktop.Storage.Backend = config.ModeLocal, config.StorageLocal

	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want int
	}{{"cloud", cloud, 1}, {"local with a database", localDB, 1}, {"desktop", desktop, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			backend := testutil.NewFakeBackend()
			opts := snapshotStoreOptions(capability.Of(tc.cfg), backend)
			if got := savedSnapshots(t, opts, backend); got != tc.want {
				t.Errorf("snapshots in the database = %d, want %d", got, tc.want)
			}
		})
	}

	if opts := snapshotStoreOptions(capability.Of(cloud), nil); len(opts) != 0 {
		t.Errorf("a store capability without a backend produced %d options", len(opts))
	}
}
