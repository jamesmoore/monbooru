package relations

import (
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
)

// PhashStored must follow every phash write so a built tree stays in step
// with the column.
func PhashStored(database *db.DB, id, phash int64) {
	tree := DefaultRegistry.Lookup(database)
	if tree == nil || !tree.Built() {
		return
	}
	tree.Insert(id, phash)
	if !IncrementalProbeEnabled.Load() {
		return
	}
	distance := int(IncrementalProbeDistance.Load())
	if err := incrementalProbe(database, tree, id, phash, distance); err != nil {
		logx.Debugf("incremental probe %d: %v", id, err)
	}
}

func PhashCleared(database *db.DB, id int64) {
	if tree := DefaultRegistry.Lookup(database); tree != nil && tree.Built() {
		tree.Remove(id)
	}
}

func PhashSink(database *db.DB) gallery.PhashSink {
	return func(id int64, phash *int64) {
		if phash == nil {
			PhashCleared(database, id)
			return
		}
		PhashStored(database, id, *phash)
	}
}

func (t *BKTree) EnsureBuilt(database *db.DB) error {
	if t.Built() {
		return nil
	}
	return t.BuildFromDB(database)
}

type BKTree struct {
	mu       sync.RWMutex
	root     *bkNode
	idIndex  map[int64]int64 // id -> phash
	built    atomic.Bool
	lastUsed atomic.Int64
}

type bkNode struct {
	phash int64
	ids   []int64
	// A slice, not a map: fanout is at most 65 and usually a handful.
	children []bkEdge
}

type bkEdge struct {
	dist int
	node *bkNode
}

func (n *bkNode) child(dist int) *bkNode {
	for i := range n.children {
		if n.children[i].dist == dist {
			return n.children[i].node
		}
	}
	return nil
}

func NewBKTree() *BKTree { return &BKTree{idIndex: make(map[int64]int64)} }

func (t *BKTree) Built() bool { return t.built.Load() }

func (t *BKTree) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetLocked()
}

func (t *BKTree) ReleaseIdle(after time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.built.Load() || time.Since(time.Unix(0, t.lastUsed.Load())) < after {
		return false
	}
	t.resetLocked()
	return true
}

func (t *BKTree) resetLocked() {
	t.root = nil
	t.idIndex = make(map[int64]int64)
	t.built.Store(false)
}

func (t *BKTree) BuildFromDB(database *db.DB) error {
	rows, err := database.Read.Query(`SELECT id, phash FROM images WHERE phash IS NOT NULL`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = nil
	t.idIndex = make(map[int64]int64)
	for rows.Next() {
		var id, phash int64
		if err := rows.Scan(&id, &phash); err != nil {
			return err
		}
		t.insertLocked(id, phash)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	t.lastUsed.Store(time.Now().UnixNano())
	t.built.Store(true)
	return nil
}

func (t *BKTree) Insert(id, phash int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.idIndex[id]; ok {
		t.removeIDLocked(id, prev)
	}
	t.insertLocked(id, phash)
}

func (t *BKTree) Remove(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	phash, ok := t.idIndex[id]
	if !ok {
		return
	}
	t.removeIDLocked(id, phash)
}

// SearchWithinDistance reports false when no built tree answered: the
// tree can be dropped after EnsureBuilt, so the caller must fall back
// rather than read "no matches".
func (t *BKTree) SearchWithinDistance(query int64, d int) ([]int64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	t.lastUsed.Store(time.Now().UnixNano())
	var out []int64
	if t.root != nil {
		t.searchLocked(t.root, query, d, &out)
	}
	return out, t.built.Load()
}

func (t *BKTree) insertLocked(id, phash int64) {
	t.idIndex[id] = phash
	if t.root == nil {
		t.root = &bkNode{phash: phash, ids: []int64{id}}
		return
	}
	cur := t.root
	for {
		if cur.phash == phash {
			cur.ids = append(cur.ids, id)
			return
		}
		dist := hammingDistance(cur.phash, phash)
		if child := cur.child(dist); child != nil {
			cur = child
			continue
		}
		cur.children = append(cur.children, bkEdge{dist: dist, node: &bkNode{phash: phash, ids: []int64{id}}})
		return
	}
}

func (t *BKTree) removeIDLocked(id, phash int64) {
	delete(t.idIndex, id)
	// An emptied node stays in place rather than rewriting the tree on
	// every delete.
	cur := t.root
	for cur != nil {
		if cur.phash == phash {
			if i := slices.Index(cur.ids, id); i >= 0 {
				cur.ids = slices.Delete(cur.ids, i, i+1)
			}
			return
		}
		cur = cur.child(hammingDistance(cur.phash, phash))
	}
}

func (t *BKTree) searchLocked(node *bkNode, query int64, d int, out *[]int64) {
	dist := hammingDistance(node.phash, query)
	if dist <= d {
		*out = append(*out, node.ids...)
	}
	lo := max(dist-d, 0)
	hi := dist + d
	for _, edge := range node.children {
		if edge.dist < lo || edge.dist > hi {
			continue
		}
		t.searchLocked(edge.node, query, d, out)
	}
}

func hammingDistance(a, b int64) int { return bits.OnesCount64(uint64(a) ^ uint64(b)) }

type Registry struct {
	mu    sync.RWMutex
	trees map[*db.DB]*BKTree
}

var DefaultRegistry = &Registry{trees: map[*db.DB]*BKTree{}}

func (r *Registry) Register(database *db.DB, tree *BKTree) {
	r.mu.Lock()
	r.trees[database] = tree
	r.mu.Unlock()
}

func (r *Registry) Unregister(database *db.DB) {
	r.mu.Lock()
	delete(r.trees, database)
	r.mu.Unlock()
}

func (r *Registry) Lookup(database *db.DB) *BKTree {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.trees[database]
}
