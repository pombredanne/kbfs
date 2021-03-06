// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"testing"
	"time"

	"golang.org/x/net/context"
)

func testDirtyBcachePut(t *testing.T, id BlockID, dirtyBcache DirtyBlockCache) {
	block := NewFileBlock()
	ptr := BlockPointer{ID: id}
	branch := MasterBranch

	// put the block
	if err := dirtyBcache.Put(ptr, branch, block); err != nil {
		t.Errorf("Got error on Put for block %s: %v", id, err)
	}

	// make sure we can get it successfully
	if block2, err := dirtyBcache.Get(ptr, branch); err != nil {
		t.Errorf("Got error on get for block %s: %v", id, err)
	} else if block2 != block {
		t.Errorf("Got back unexpected block: %v", block2)
	}

	// make sure its dirty status is right
	if !dirtyBcache.IsDirty(ptr, branch) {
		t.Errorf("Block %s unexpectedly not dirty", id)
	}
}

func testExpectedMissingDirty(t *testing.T, id BlockID,
	dirtyBcache DirtyBlockCache) {
	expectedErr := NoSuchBlockError{id}
	ptr := BlockPointer{ID: id}
	if _, err := dirtyBcache.Get(ptr, MasterBranch); err == nil {
		t.Errorf("No expected error on 1st get: %v", err)
	} else if err != expectedErr {
		t.Errorf("Got unexpected error on 1st get: %v", err)
	}
}

func TestDirtyBcachePut(t *testing.T) {
	dirtyBcache := NewDirtyBlockCacheStandard(&wallClock{}, testLoggerMaker(t),
		5<<20, 10<<20)
	defer dirtyBcache.Shutdown()
	testDirtyBcachePut(t, fakeBlockID(1), dirtyBcache)
}

func TestDirtyBcachePutDuplicate(t *testing.T) {
	dirtyBcache := NewDirtyBlockCacheStandard(&wallClock{}, testLoggerMaker(t),
		5<<20, 10<<20)
	defer dirtyBcache.Shutdown()
	id1 := fakeBlockID(1)

	// Dirty a specific reference nonce, and make sure the
	// original is still not found.
	newNonce := BlockRefNonce([8]byte{1, 0, 0, 0, 0, 0, 0, 0})
	newNonceBlock := NewFileBlock()
	bp1 := BlockPointer{ID: id1}
	bp2 := BlockPointer{
		ID:           id1,
		BlockContext: BlockContext{RefNonce: newNonce},
	}
	err := dirtyBcache.Put(bp2, MasterBranch, newNonceBlock)
	if err != nil {
		t.Errorf("Unexpected error on PutDirty: %v", err)
	}

	cleanBranch := MasterBranch
	testExpectedMissingDirty(t, id1, dirtyBcache)
	if !dirtyBcache.IsDirty(bp2, cleanBranch) {
		t.Errorf("New refnonce block is now unexpectedly clean")
	}

	// Then dirty a different branch, and make sure the
	// original is still clean
	newBranch := BranchName("dirtyBranch")
	newBranchBlock := NewFileBlock()
	err = dirtyBcache.Put(bp1, newBranch, newBranchBlock)
	if err != nil {
		t.Errorf("Unexpected error on PutDirty: %v", err)
	}

	// make sure the original dirty status is right
	testExpectedMissingDirty(t, id1, dirtyBcache)
	if !dirtyBcache.IsDirty(bp2, cleanBranch) {
		t.Errorf("New refnonce block is now unexpectedly clean")
	}
	if !dirtyBcache.IsDirty(bp1, newBranch) {
		t.Errorf("New branch block is now unexpectedly clean")
	}
}

func TestDirtyBcacheDelete(t *testing.T) {
	dirtyBcache := NewDirtyBlockCacheStandard(&wallClock{}, testLoggerMaker(t),
		5<<20, 10<<20)
	defer dirtyBcache.Shutdown()

	id1 := fakeBlockID(1)
	testDirtyBcachePut(t, id1, dirtyBcache)
	newBranch := BranchName("dirtyBranch")
	newBranchBlock := NewFileBlock()
	err := dirtyBcache.Put(BlockPointer{ID: id1}, newBranch, newBranchBlock)
	if err != nil {
		t.Errorf("Unexpected error on PutDirty: %v", err)
	}

	dirtyBcache.Delete(BlockPointer{ID: id1}, MasterBranch)
	testExpectedMissingDirty(t, id1, dirtyBcache)
	if !dirtyBcache.IsDirty(BlockPointer{ID: id1}, newBranch) {
		t.Errorf("New branch block is now unexpectedly clean")
	}
}

func TestDirtyBcacheRequestPermission(t *testing.T) {
	bufSize := int64(5)
	dirtyBcache := NewDirtyBlockCacheStandard(&wallClock{}, testLoggerMaker(t),
		bufSize, bufSize*2)
	defer dirtyBcache.Shutdown()
	blockedChan := make(chan int64)
	dirtyBcache.blockedChanForTesting = blockedChan
	ctx := context.Background()

	// The first write should get immediate permission.
	c1, err := dirtyBcache.RequestPermissionToDirty(ctx, bufSize*2+1)
	if err != nil {
		t.Fatalf("Request permission error: %v", err)
	}
	<-c1
	// Now the unsynced buffer is full
	if !dirtyBcache.ShouldForceSync() {
		t.Fatalf("Unsynced not full after a request")
	}
	// Not blocked
	if blockedSize := <-blockedChan; blockedSize != -1 {
		t.Fatalf("Wrong blocked size: %d", blockedSize)
	}

	// The next request should block
	c2, err := dirtyBcache.RequestPermissionToDirty(ctx, bufSize)
	if err != nil {
		t.Fatalf("Request permission error: %v", err)
	}
	if blockedSize := <-blockedChan; blockedSize != bufSize {
		t.Fatalf("Wrong blocked size: %d", blockedSize)
	}
	select {
	case <-c2:
		t.Fatalf("Request should be blocked")
	default:
	}

	// Let's say the actual number of unsynced bytes for c1 was double
	dirtyBcache.UpdateUnsyncedBytes(4*bufSize+2, false)
	// Now release the previous bytes
	dirtyBcache.UpdateUnsyncedBytes(-(2*bufSize + 1), false)

	// Request 2 should still be blocked.  (This check isn't
	// fool-proof, since it doesn't necessarily give time for the
	// background thread to run.)
	select {
	case <-c2:
		t.Fatalf("Request should be blocked")
	default:
	}

	// Finish syncing most of the blocks, but c2 is still blocked
	// because we haven't finished the sync yet (hence the sync buffer
	// size hasn't increased yet).
	dirtyBcache.BlockSyncFinished(2*bufSize + 1)
	dirtyBcache.BlockSyncFinished(bufSize)
	if !dirtyBcache.ShouldForceSync() {
		t.Fatalf("Total not full before sync finishes")
	}
	select {
	case <-c2:
		t.Fatalf("Request should be blocked")
	default:
	}

	// Finally, finish off the sync, which should unblock c2
	dirtyBcache.BlockSyncFinished(bufSize + 1)
	dirtyBcache.SyncFinished(4*bufSize + 2)
	if dirtyBcache.ShouldForceSync() {
		t.Fatalf("Buffers still full after sync finished")
	}

	if blockedSize := <-blockedChan; blockedSize != -1 {
		t.Fatalf("Wrong blocked size: %d", blockedSize)
	}
	<-c2
}

func TestDirtyBcacheCalcBackpressure(t *testing.T) {
	bufSize := int64(10)
	clock, now := newTestClockAndTimeNow()
	dirtyBcache := NewDirtyBlockCacheStandard(clock, testLoggerMaker(t),
		bufSize, bufSize*2)
	defer dirtyBcache.Shutdown()
	// no backpressure yet
	bp := dirtyBcache.calcBackpressure(now, now.Add(11*time.Second))
	if bp != 0 {
		t.Fatalf("Unexpected backpressure before unsyned bytes: %d", bp)
	}

	// still less
	dirtyBcache.UpdateUnsyncedBytes(9, false)
	bp = dirtyBcache.calcBackpressure(now, now.Add(11*time.Second))
	if bp != 0 {
		t.Fatalf("Unexpected backpressure before unsyned bytes: %d", bp)
	}

	// Now make 11 unsynced bytes, or 10% of the overage
	dirtyBcache.UpdateUnsyncedBytes(2, false)
	bp = dirtyBcache.calcBackpressure(now, now.Add(11*time.Second))
	if g, e := bp, 1*time.Second; g != e {
		t.Fatalf("Got backpressure %s, expected %s", g, e)
	}

	// Now completely fill the buffer
	dirtyBcache.UpdateUnsyncedBytes(9, false)
	bp = dirtyBcache.calcBackpressure(now, now.Add(11*time.Second))
	if g, e := bp, 10*time.Second; g != e {
		t.Fatalf("Got backpressure %s, expected %s", g, e)
	}

	// Now advance the clock, we should see the same bp deadline
	clock.Add(5 * time.Second)
	bp = dirtyBcache.calcBackpressure(now, now.Add(11*time.Second))
	if g, e := bp, 5*time.Second; g != e {
		t.Fatalf("Got backpressure %s, expected %s", g, e)
	}
}
