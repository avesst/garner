package publish

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/store"
)

// gate pauses the first call through pass after it is armed, until
// released, so a test can run other work at that point.
type gate struct {
	armed            atomic.Bool
	reached, release chan struct{}
}

func newGate() *gate {
	return &gate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) pass() {
	if g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		<-g.release
	}
}

// nameGated pauses in Name. GC first calls it for the pool sweep: after
// its package transaction committed, before it removes blobs.
type nameGated struct {
	deb.Format
	g *gate
}

func (f nameGated) Name() string { f.g.pass(); return f.Format.Name() }

// detectGated pauses in Detect, which ingest calls after Put and before
// its transaction.
type detectGated struct {
	deb.Format
	g *gate
}

func (f detectGated) Detect(head []byte) bool { f.g.pass(); return f.Format.Detect(head) }

var raceRequest = ingest.Request{Owner: "team", Origin: "upload", OriginRef: "t", VerifyStatus: "n/a"}

// removedUpload ingests data and removes it again before any publish, so
// GC will collect its package and blob. It returns the blob's file name.
func removedUpload(t *testing.T, f *fixture, data []byte) string {
	t.Helper()
	ctx := context.Background()
	r, err := f.in.Ingest(ctx, bytes.NewReader(data), raceRequest)
	if err != nil {
		t.Fatal(err)
	}
	err = f.in.Store.Tx(ctx, func(q *store.Queries) error {
		_, err := q.SetState(ctx, r.Package.ID, r.Distribution, store.Removed)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := f.pub.Blobs.Path(r.Package.BlobSHA256)
	return path
}

func checkServable(t *testing.T, f *fixture, blobPath string) {
	t.Helper()
	if _, err := os.Stat(blobPath); err != nil {
		t.Errorf("blob of the re-uploaded package: %v", err)
	}
	if _, err := f.pub.Publish(context.Background()); err != nil {
		t.Errorf("publish after the race: %v", err)
	}
}

// An upload of a blob's content that commits while GC is collecting the
// blob keeps it.
func TestGCKeepsBlobUploadedDuringCollection(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.cfg.Storage.GCGrace = 0
	g := newGate()
	reg, _ := format.NewRegistry(nameGated{g: g})
	f.pub.Formats = reg
	data := debtest.Build("app", "1.0", "amd64", "payload")
	blobPath := removedUpload(t, f, data)

	g.armed.Store(true)
	done := make(chan *GCResult)
	go func() {
		res, err := f.pub.GC(ctx)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	<-g.reached
	if _, err := f.in.Ingest(ctx, bytes.NewReader(data), raceRequest); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if res := <-done; res != nil && res.Blobs != 0 {
		t.Errorf("GC removed %d blobs, want 0", res.Blobs)
	}
	checkServable(t, f, blobPath)
}

// An upload whose Put found the file present, and which GC then removed
// before the upload's transaction, restores it.
func TestUploadRestoresBlobCollectedAfterPut(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.cfg.Storage.GCGrace = 0
	g := newGate()
	reg, _ := format.NewRegistry(detectGated{g: g})
	f.in.Formats = reg
	data := debtest.Build("app", "1.0", "amd64", "payload")
	blobPath := removedUpload(t, f, data)

	g.armed.Store(true)
	done := make(chan error)
	go func() {
		_, err := f.in.Ingest(ctx, bytes.NewReader(data), raceRequest)
		done <- err
	}()
	<-g.reached
	res, err := f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 1 {
		t.Fatalf("GC removed %d blobs, want 1: the race did not happen", res.Blobs)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	checkServable(t, f, blobPath)
}
