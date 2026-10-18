package tests_test

import (
	"testing"

	"gorm.io/gorm/clause"
	. "gorm.io/gorm/utils/tests"
)

// CircularReferenceA / CircularReferenceB reference each other with nullable
// foreign keys.
type CircularReferenceA struct {
	ID  uint
	BID *uint
	B   *CircularReferenceB `gorm:"foreignKey:BID"`
}

type CircularReferenceB struct {
	ID  uint
	AID *uint
	A   *CircularReferenceA `gorm:"foreignKey:AID"`
}

// CircularReferenceEmployee is a self referential model (manager / reports).
type CircularReferenceEmployee struct {
	ID        uint
	Name      string
	ManagerID *uint
	Manager   *CircularReferenceEmployee   `gorm:"foreignKey:ManagerID"`
	Reports   []*CircularReferenceEmployee `gorm:"foreignKey:ManagerID"`
}

type CircularReferenceNotNullA struct {
	ID  uint
	CID *uint
	C   *CircularReferenceNotNullC `gorm:"foreignKey:CID"`
}

type CircularReferenceNotNullC struct {
	ID  uint
	AID uint `gorm:"not null"`
	A   *CircularReferenceNotNullA `gorm:"foreignKey:AID"`
}

func prepareCircularReferenceTables(t *testing.T) {
	t.Helper()
	if err := DB.AutoMigrate(
		&CircularReferenceA{}, &CircularReferenceB{},
		&CircularReferenceEmployee{},
		&CircularReferenceNotNullA{}, &CircularReferenceNotNullC{},
	); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
}

func cleanupCircularReferenceTables(t *testing.T) {
	t.Helper()
	DB.Migrator().DropTable(
		&CircularReferenceNotNullC{}, &CircularReferenceNotNullA{},
		&CircularReferenceEmployee{},
		&CircularReferenceB{}, &CircularReferenceA{},
	)
}

func TestCreateCircularBelongsReference(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	a := &CircularReferenceA{}
	b := &CircularReferenceB{}
	a.B = b
	b.A = a

	if err := DB.Create(a).Error; err != nil {
		t.Fatalf("failed to create circular objects: %v", err)
	}

	if a.ID == 0 || b.ID == 0 {
		t.Fatalf("primary keys must be assigned, got a=%d b=%d", a.ID, b.ID)
	}
	if a.BID == nil || *a.BID != b.ID {
		t.Fatalf("a.BID = %v, wants %d", derefUint(a.BID), b.ID)
	}
	if b.AID == nil || *b.AID != a.ID {
		t.Fatalf("b.AID = %v, wants %d", derefUint(b.AID), a.ID)
	}

	var aRow CircularReferenceA
	if err := DB.First(&aRow, a.ID).Error; err != nil {
		t.Fatalf("failed to load a: %v", err)
	}
	var bRow CircularReferenceB
	if err := DB.First(&bRow, b.ID).Error; err != nil {
		t.Fatalf("failed to load b: %v", err)
	}
	if aRow.BID == nil || *aRow.BID != b.ID {
		t.Errorf("db a.BID = %v, wants %d", derefUint(aRow.BID), b.ID)
	}
	if bRow.AID == nil || *bRow.AID != a.ID {
		t.Errorf("db b.AID = %v, wants %d", derefUint(bRow.AID), a.ID)
	}
}

func TestCreateCircularReferenceSingleRow(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	e := &CircularReferenceEmployee{Name: "self"}
	e.Manager = e

	if err := DB.Create(e).Error; err != nil {
		t.Fatalf("failed to create self referencing row: %v", err)
	}

	var row CircularReferenceEmployee
	if err := DB.First(&row, e.ID).Error; err != nil {
		t.Fatalf("failed to load row: %v", err)
	}
	if row.ManagerID == nil || *row.ManagerID != e.ID {
		t.Fatalf("manager_id = %v, wants %d", derefUint(row.ManagerID), e.ID)
	}
}

func TestCreateCircularReferenceFromBelongsSide(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	dev := &CircularReferenceEmployee{Name: "dev"}
	boss := &CircularReferenceEmployee{Name: "boss", Manager: dev}
	dev.Reports = []*CircularReferenceEmployee{boss}

	if err := DB.Create(boss).Error; err != nil {
		t.Fatalf("failed to create: %v", err)
	}

	var count int64
	DB.Model(&CircularReferenceEmployee{}).Count(&count)
	AssertEqual(t, count, int64(2))

	var bossRow CircularReferenceEmployee
	DB.First(&bossRow, boss.ID)
	if bossRow.ManagerID == nil || *bossRow.ManagerID != dev.ID {
		t.Fatalf("boss.manager_id = %v, wants %d", derefUint(bossRow.ManagerID), dev.ID)
	}
}

// cyclic pair plus an unrelated row in the same batch: the unrelated row keeps
// its insertion position and its NULL foreign key
func TestCreateCircularReferenceMixedBatch(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	e1 := &CircularReferenceEmployee{Name: "e1"}
	e2 := &CircularReferenceEmployee{Name: "e2"}
	e1.Manager = e2
	e2.Manager = e1
	plain := &CircularReferenceEmployee{Name: "plain"}

	if err := DB.Create([]*CircularReferenceEmployee{e1, e2, plain}).Error; err != nil {
		t.Fatalf("failed to create batch: %v", err)
	}

	var count int64
	DB.Model(&CircularReferenceEmployee{}).Count(&count)
	AssertEqual(t, count, int64(3))

	if !(e1.ID < e2.ID && e2.ID < plain.ID) {
		t.Errorf("insertion order changed: e1=%d e2=%d plain=%d", e1.ID, e2.ID, plain.ID)
	}

	var plainRow CircularReferenceEmployee
	DB.First(&plainRow, plain.ID)
	if plainRow.ManagerID != nil {
		t.Errorf("plain.ManagerID = %d, wants NULL", *plainRow.ManagerID)
	}
}

// a NOT NULL circular foreign key must fail during the create and roll back
// every row inserted by the operation
func TestCreateCircularReferenceNotNullFails(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	a := &CircularReferenceNotNullA{}
	c := &CircularReferenceNotNullC{}
	a.C = c
	c.A = a

	if err := DB.Create(a).Error; err == nil {
		t.Fatalf("creating a NOT NULL cyclic graph must fail")
	}

	var aCount, cCount int64
	DB.Model(&CircularReferenceNotNullA{}).Count(&aCount)
	DB.Model(&CircularReferenceNotNullC{}).Count(&cCount)
	AssertEqual(t, aCount, int64(0))
	AssertEqual(t, cCount, int64(0))
}

// inserts and backfills run in the caller supplied transaction
func TestCreateCircularReferenceInCallerTransaction(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	// rollback
	func() {
		tx := DB.Begin()
		a := &CircularReferenceA{}
		b := &CircularReferenceB{}
		a.B = b
		b.A = a
		if err := tx.Create(a).Error; err != nil {
			t.Fatalf("failed to create in tx: %v", err)
		}
		tx.Rollback()

		var count int64
		DB.Model(&CircularReferenceB{}).Count(&count)
		AssertEqual(t, count, int64(0))
	}()

	// commit
	tx := DB.Begin()
	a := &CircularReferenceA{}
	b := &CircularReferenceB{}
	a.B = b
	b.A = a
	if err := tx.Create(a).Error; err != nil {
		t.Fatalf("failed to create in tx: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}

	var bRow CircularReferenceB
	DB.First(&bRow, b.ID)
	if bRow.AID == nil || *bRow.AID != a.ID {
		t.Fatalf("backfill not committed, b.AID = %v", derefUint(bRow.AID))
	}
}

// rolling back to a savepoint taken before the create undoes the inserts and
// the backfill UPDATE alike (they share the same transaction)
func TestCreateCircularReferenceSavePointRollback(t *testing.T) {
	if DB.Dialector.Name() == "sqlserver" {
		t.Skip("savepoint API is not supported by the sqlserver dialector")
	}

	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	tx := DB.Begin()
	if err := tx.SavePoint("circular_sp").Error; err != nil {
		t.Fatalf("savepoint: %v", err)
	}

	a := &CircularReferenceA{}
	b := &CircularReferenceB{}
	a.B = b
	b.A = a
	if err := tx.Create(a).Error; err != nil {
		t.Fatalf("failed to create: %v", err)
	}

	if err := tx.RollbackTo("circular_sp").Error; err != nil {
		t.Fatalf("rollbackto: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}

	var aCount, bCount int64
	DB.Model(&CircularReferenceA{}).Count(&aCount)
	DB.Model(&CircularReferenceB{}).Count(&bCount)
	AssertEqual(t, aCount, int64(0))
	AssertEqual(t, bCount, int64(0))
}

// caller supplied upserts keep the legacy conflict handling: a cyclic graph
// with unresolved foreign keys is handled without the new backfill path and
// therefore surfaces the same constraint error as before instead of silently
// changing behavior
func TestCreateCircularReferenceOnConflictLegacy(t *testing.T) {
	prepareCircularReferenceTables(t)
	defer cleanupCircularReferenceTables(t)

	a := &CircularReferenceA{}
	b := &CircularReferenceB{}
	a.B = b
	b.A = a

	// legacy behavior on databases that enforce foreign keys is to raise; on
	// databases without enforcement it leaves exactly one row per table and
	// never performs extra backfill UPDATEs
	err := DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(a).Error
	if err == nil {
		var aCount, bCount int64
		DB.Model(&CircularReferenceA{}).Count(&aCount)
		DB.Model(&CircularReferenceB{}).Count(&bCount)
		AssertEqual(t, aCount, int64(1))
		AssertEqual(t, bCount, int64(1))
	}
}

func derefUint(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}
