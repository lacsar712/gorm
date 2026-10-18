package callbacks

import (
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Circular foreign key support.
//
// When a single Create call persists a graph of objects whose foreign keys
// point back at objects that are still being inserted (for example A belongs
// to B while B belongs to A), the back edges cannot carry a resolved foreign
// key value at INSERT time. The create callbacks detect those edges, insert
// the rows with the unresolved foreign key columns set to NULL and backfill
// the real primary keys with UPDATE statements once every involved row
// exists.
//
// All work happens inside the create callback chain and reuses the statement's
// connection pool, so inserts and backfills run in the same transaction
// (including transactions opened by the caller). When no circular reference
// is involved the tracker stays inert and every callback behaves exactly as
// before.

const (
	circularReferenceStoreKey = "gorm:circular_reference"
	circularFrameStoreKey     = "gorm:circular_reference_frame"
	circularDisabledStoreKey  = "gorm:circular_reference_disabled"
	// circularOperationDisabledKey marks a whole create operation (including its
	// nested association saves) as a caller supplied upsert that keeps the
	// legacy conflict handling and must not use circular backfills.
	circularOperationDisabledKey = "gorm:circular_reference_operation_disabled"
	// associationSaveStoreKey marks statements issued from the association
	// callbacks. Such statements always carry an internal ON CONFLICT clause
	// but must still participate in circular reference handling, whereas a
	// user supplied clause.OnConflict keeps the legacy upsert behavior.
	associationSaveStoreKey = "gorm:save_association"
)

// circularReferenceBackfill describes one deferred foreign key assignment:
// the foreign key columns living on holder have to be updated to the primary
// key values of target after both rows are inserted.
type circularReferenceBackfill struct {
	schema       *schema.Schema
	targetSchema *schema.Schema
	holder       reflect.Value
	target       reflect.Value
	refs         []*schema.Reference
}

// circularReference is shared (through the statement Settings) by every
// nested Create that belongs to the same outermost Create call.
type circularReference struct {
	depth int
	// creating counts create callbacks still running for each object.
	creating map[reflect.Value]int
	// inserted records objects whose row has already been inserted.
	inserted map[reflect.Value]bool
	// nullColumns records the foreign key columns that must be written as NULL
	// when a given object is inserted because they are part of a circular
	// reference whose referenced row does not exist yet.
	omits map[reflect.Value]map[string]bool
	// pending collects backfills waiting for one side of the edge to finish.
	pending []*circularReferenceBackfill
}

func loadCircularReference(db *gorm.DB) *circularReference {
	if v, ok := db.Get(circularReferenceStoreKey); ok {
		if c, ok := v.(*circularReference); ok {
			return c
		}
	}
	return nil
}

// circularKey returns a stable identity for an object following the same
// rules as the association visit map: the address of the addressable struct.
func circularKey(v reflect.Value) (reflect.Value, bool) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}

	if v.Kind() == reflect.Struct && v.CanAddr() {
		return v.Addr(), true
	}
	return reflect.Value{}, false
}

func circularFrameKeys(rv reflect.Value) []reflect.Value {
	switch rv.Kind() {
	case reflect.Struct:
		if key, ok := circularKey(rv); ok {
			return []reflect.Value{key}
		}
	case reflect.Slice, reflect.Array:
		keys := make([]reflect.Value, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			if reflect.Indirect(elem).Kind() != reflect.Struct {
				break
			}
			if key, ok := circularKey(elem); ok {
				keys = append(keys, key)
			}
		}
		return keys
	}
	return nil
}

func circularPrimaryKeysKnown(db *gorm.DB, s *schema.Schema, key reflect.Value) bool {
	elem := key.Elem()
	for _, field := range s.PrimaryFields {
		if _, isZero := field.ValueOf(db.Statement.Context, elem); isZero {
			return false
		}
	}
	return true
}

const (
	circularStateNormal      = 0 // regular association handling
	circularStateDefer       = 1 // omit the foreign key at insert time and backfill it
	circularStateUseExisting = 2 // the related object is inserted elsewhere in the same batch and its primary key is known
)

// circularBelongsToState classifies a belongs to target encountered while
// saving the current object.
func circularBelongsToState(db *gorm.DB, s *schema.Schema, elem reflect.Value) int {
	c := loadCircularReference(db)
	if c == nil {
		return circularStateNormal
	}

	key, ok := circularKey(elem)
	if !ok || c.creating[key] == 0 {
		return circularStateNormal
	}

	// The row already exists (possibly through a re-entrant association save):
	// the legacy ON CONFLICT / copy primary key behavior stays valid.
	if c.inserted[key] {
		return circularStateNormal
	}

	if circularPrimaryKeysKnown(db, s, key) {
		// Primary keys are known before insert: copy the foreign key directly,
		// the in-progress frame is responsible for the actual insert.
		return circularStateUseExisting
	}

	return circularStateDefer
}

// circularHasChildState classifies a has one / has many child encountered
// while saving the current object.
func circularHasChildState(db *gorm.DB, s *schema.Schema, elem reflect.Value) int {
	c := loadCircularReference(db)
	if c == nil {
		return circularStateNormal
	}

	key, ok := circularKey(elem)
	if !ok || c.creating[key] == 0 {
		return circularStateNormal
	}

	// The child row already exists, the legacy ON CONFLICT DO UPDATE handling
	// keeps the foreign key in sync.
	if c.inserted[key] {
		return circularStateNormal
	}

	// Child is still waiting to be inserted higher up the stack: its foreign
	// key cannot be written yet, defer it.
	return circularStateDefer
}

// circularReferencePairs selects the reference pairs that carry the foreign
// key for a non polymorphic belongs to / has one / has many relation.
func circularReferencePairs(rel *schema.Relationship, ownPrimaryKey bool) []*schema.Reference {
	if rel.Polymorphic != nil {
		return nil
	}

	pairs := make([]*schema.Reference, 0, len(rel.References))
	for _, ref := range rel.References {
		if ref.PrimaryKey == nil {
			continue
		}
		if ownPrimaryKey && ref.OwnPrimaryKey {
			pairs = append(pairs, ref)
		} else if !ownPrimaryKey && !ref.OwnPrimaryKey {
			pairs = append(pairs, ref)
		}
	}
	return pairs
}

// circularDeferReference registers a backfill for a circular reference edge
// and remembers the foreign key columns to write as NULL when holder is
// inserted.
func circularDeferReference(db *gorm.DB, holderSchema, targetSchema *schema.Schema, holder, target reflect.Value, pairs []*schema.Reference) {
	if len(pairs) == 0 {
		return
	}

	c := loadCircularReference(db)
	if c == nil {
		return
	}

	holderKey, ok := circularKey(holder)
	if !ok {
		return
	}
	targetKey, ok := circularKey(target)
	if !ok {
		return
	}

	// Merge references when several relations connect the same pair of
	// objects through different foreign key columns.
	for _, backfill := range c.pending {
		if backfill.holder == holderKey && backfill.target == targetKey {
		pairLoop:
			for _, ref := range pairs {
				for _, existing := range backfill.refs {
					if existing.ForeignKey.DBName == ref.ForeignKey.DBName {
						continue pairLoop
					}
				}
				backfill.refs = append(backfill.refs, ref)
				c.omits[holderKey][ref.ForeignKey.DBName] = true
			}
			return
		}
	}

	c.pending = append(c.pending, &circularReferenceBackfill{
		schema:       holderSchema,
		targetSchema: targetSchema,
		holder:       holderKey,
		target:       targetKey,
		refs:         pairs,
	})

	if c.omits == nil {
		c.omits = map[reflect.Value]map[string]bool{}
	}
	omitColumns := c.omits[holderKey]
	if omitColumns == nil {
		omitColumns = map[string]bool{}
		c.omits[holderKey] = omitColumns
	}
	for _, ref := range pairs {
		omitColumns[ref.ForeignKey.DBName] = true
	}
}

// CircularReferenceBegin opens a tracking frame for the statement's objects.
// Statements using a caller provided clause.OnConflict keep the legacy upsert
// behavior; only internal association saves may participate while carrying an
// ON CONFLICT clause.
func CircularReferenceBegin(db *gorm.DB) {
	if db.Error != nil || db.Statement.Schema == nil {
		return
	}

	// A caller supplied upsert disables circular backfills for the whole create
	// operation; the setting is propagated to nested association statements.
	if v, ok := db.Get(circularOperationDisabledKey); ok && v != nil {
		db.InstanceSet(circularDisabledStoreKey, true)
		return
	}

	if _, ok := db.Statement.Clauses["ON CONFLICT"]; ok {
		if _, marked := db.Get(associationSaveStoreKey); !marked {
			db.Set(circularOperationDisabledKey, true)
			db.InstanceSet(circularDisabledStoreKey, true)
			return
		}
	}

	keys := circularFrameKeys(db.Statement.ReflectValue)
	if len(keys) == 0 {
		db.InstanceSet(circularDisabledStoreKey, true)
		return
	}

	c := loadCircularReference(db)
	if c == nil {
		c = &circularReference{
			creating: map[reflect.Value]int{},
			inserted: map[reflect.Value]bool{},
		}
		db.Set(circularReferenceStoreKey, c)
	}
	if c.creating == nil {
		c.creating = map[reflect.Value]int{}
	}
	if c.inserted == nil {
		c.inserted = map[reflect.Value]bool{}
	}

	c.depth++
	for _, key := range keys {
		c.creating[key]++
	}
	db.InstanceSet(circularFrameStoreKey, keys)
}

// CircularReferenceAfterInsert marks the frame objects as inserted and
// backfills every reference that has become resolvable.
func CircularReferenceAfterInsert(db *gorm.DB) {
	if db.Error != nil || db.DryRun {
		return
	}

	if disabled, _ := db.InstanceGet(circularDisabledStoreKey); disabled != nil {
		return
	}

	c := loadCircularReference(db)
	if c == nil {
		return
	}

	if v, ok := db.InstanceGet(circularFrameStoreKey); ok {
		if keys, ok := v.([]reflect.Value); ok {
			for _, key := range keys {
				c.inserted[key] = true
			}
		}
	}

	resolveCircularReferences(db)
}

// CircularReferenceResolve backfills references that became resolvable while
// the after associations were saved. It runs before AfterCreate so hooks do
// not observe unresolved foreign keys as the final state.
func CircularReferenceResolve(db *gorm.DB) {
	if db.Error != nil || db.DryRun {
		return
	}

	if disabled, _ := db.InstanceGet(circularDisabledStoreKey); disabled != nil {
		return
	}

	resolveCircularReferences(db)
}

// CircularReferenceEnd closes the tracking frame. Backfills are resolved
// before the frame is fully released so that values changed by AfterCreate
// hooks are taken into account.
func CircularReferenceEnd(db *gorm.DB) {
	if disabled, _ := db.InstanceGet(circularDisabledStoreKey); disabled != nil {
		return
	}

	c := loadCircularReference(db)
	if c == nil {
		return
	}

	if v, ok := db.InstanceGet(circularFrameStoreKey); ok {
		if keys, ok := v.([]reflect.Value); ok {
			for _, key := range keys {
				if c.creating[key] > 0 {
					c.creating[key]--
				}
				if c.creating[key] == 0 {
					delete(c.creating, key)
				}
			}
		}
	}

	c.depth--

	if db.Error == nil && !db.DryRun {
		resolveCircularReferences(db)
	}

	if c.depth <= 0 {
		// The outermost create is finished: every object has had its chance to
		// be inserted, so do one last resolution. References that remain cannot
		// be resolved (for example because associations were omitted) and must
		// not change the legacy behavior.
		if db.Error == nil && !db.DryRun {
			resolveCircularReferences(db)
		}
		c.pending = nil
		c.omits = nil
		c.inserted = nil
		c.creating = nil
		c.depth = 0
	}
}

// resolveCircularReferences applies every backfill whose holder row exists and
// whose target primary keys are known.
func resolveCircularReferences(db *gorm.DB) {
	c := loadCircularReference(db)
	if c == nil || len(c.pending) == 0 {
		return
	}

	pending := c.pending[:0]
	for _, backfill := range c.pending {
		if c.inserted[backfill.holder] && circularPrimaryKeysKnown(db, backfill.targetSchema, backfill.target) {
			if err := applyCircularReferenceBackfill(db, backfill); err != nil {
				db.AddError(err)
				c.pending = append(pending, backfill)
				return
			}
			delete(c.omits, backfill.holder)
			continue
		}
		pending = append(pending, backfill)
	}
	c.pending = pending
}

// applyCircularReferenceBackfill writes the resolved foreign keys back on the
// holder object and UPDATEs the holder row through the current connection,
// staying inside the ongoing transaction.
func applyCircularReferenceBackfill(db *gorm.DB, backfill *circularReferenceBackfill) error {
	holder := backfill.holder.Interface()
	target := backfill.target.Elem()
	sets := make(map[string]interface{}, len(backfill.refs))

	for _, ref := range backfill.refs {
		value, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, target)
		if err := ref.ForeignKey.Set(db.Statement.Context, backfill.holder.Elem(), value); err != nil {
			return err
		}
		sets[ref.ForeignKey.DBName] = value
	}

	// A fresh statement inherits the current connection pool (the ongoing
	// transaction when one is in use) and never starts a nested transaction.
	// Model() must follow the NewDB session so the statement is reset before
	// SkipHooks clones it; otherwise the in-flight INSERT SQL would be reused.
	// Associations are skipped: this UPDATE only carries the foreign key.
	tx := db.Session(&gorm.Session{NewDB: true}).
		Model(holder).
		Session(&gorm.Session{SkipHooks: true}).
		Omit(clause.Associations)
	return tx.UpdateColumns(sets).Error
}

// circularNilForeignKey reports whether the given foreign key column of the
// given row must be inserted as NULL because the referenced row of the same
// Create call has not been inserted yet.
func circularNilForeignKey(stmt *gorm.Statement, row reflect.Value, dbName string) bool {
	if v, ok := stmt.Get(circularReferenceStoreKey); ok {
		if c, ok := v.(*circularReference); ok && len(c.omits) > 0 {
			if key, ok := circularKey(row); ok {
				return c.omits[key][dbName]
			}
		}
	}
	return false
}
