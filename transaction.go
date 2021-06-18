// Copyright (c) Roman Atachiants and contributors. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.

package column

import (
	"fmt"
	"sync"
	"time"

	"github.com/kelindar/bitmap"
)

// UpdateKind represents a type of an update operation.
type UpdateKind uint8

// Various update operations supported.
const (
	UpdatePut UpdateKind = iota // Put stores a value regardless of a previous value
	UpdateAdd                   // Add increments the current stored value by the amount
)

// Update represents an update operation
type Update struct {
	Kind  UpdateKind  // The type of an update operation
	Index uint32      // The index to update/delete
	Value interface{} // The value to update to
}

// --------------------------- Pool of Transactions ----------------------------

// txns represents a pool of transactions
var txns = &sync.Pool{
	New: func() interface{} {
		return &Txn{
			index:   make(bitmap.Bitmap, 0, 64),
			deletes: make(bitmap.Bitmap, 0, 64),
			inserts: make(bitmap.Bitmap, 0, 64),
			updates: make([]updateQueue, 0, 16),
			columns: make([]columnCache, 0, 16),
		}
	},
}

// aquireBitmap acquires a transaction for a transaction
func aquireTxn(owner *Collection) *Txn {
	txn := txns.Get().(*Txn)
	txn.owner = owner
	txn.columns = txn.columns[:0]
	owner.fill.Clone(&txn.index)
	return txn
}

// releaseTxn releases a transaction back to the pool
func releaseTxn(txn *Txn) {
	txns.Put(txn)
}

// --------------------------- Transaction ----------------------------

// Txn represents a transaction which supports filtering and projection.
type Txn struct {
	owner   *Collection   // The target collection
	index   bitmap.Bitmap // The filtering index
	deletes bitmap.Bitmap // The delete queue
	inserts bitmap.Bitmap // The insert queue
	updates []updateQueue // The update queue
	columns []columnCache // The column mapping
}

// Update queue represents a queue per column that contains the pending updates.
type updateQueue struct {
	name   string   // The column name
	update []Update // The update queue
}

// columnCache caches a column by its name. This speeds things up since it's a very
// common operation.
type columnCache struct {
	name string // The column name
	col  Column // The columns and its computed
}

// columnAt loads and caches the column for the transaction
func (txn *Txn) columnAt(columnName string) (Column, bool) {
	for _, v := range txn.columns {
		if v.name == columnName {
			return v.col, true
		}
	}

	// Load the column from the owner
	column, ok := txn.owner.cols.Load(columnName)
	if !ok {
		return nil, false
	}

	// Cache the loaded column for this transaction
	txn.columns = append(txn.columns, columnCache{
		name: columnName,
		col:  column,
	})
	return column, true
}

// With applies a logical AND operation to the current query and the specified index.
func (txn *Txn) With(column string, extra ...string) *Txn {
	if idx, ok := txn.columnAt(column); ok {
		idx.Intersect(&txn.index)
	} else {
		txn.index.Clear()
	}

	// go through extra indexes
	for _, e := range extra {
		if idx, ok := txn.columnAt(e); ok {
			idx.Intersect(&txn.index)
		} else {
			txn.index.Clear()
		}
	}
	return txn
}

// Without applies a logical AND NOT operation to the current query and the specified index.
func (txn *Txn) Without(column string, extra ...string) *Txn {
	if idx, ok := txn.columnAt(column); ok {
		idx.Difference(&txn.index)
	}

	// go through extra indexes
	for _, e := range extra {
		if idx, ok := txn.columnAt(e); ok {
			idx.Difference(&txn.index)
		}
	}
	return txn
}

// Union computes a union between the current query and the specified index.
func (txn *Txn) Union(column string, extra ...string) *Txn {
	if idx, ok := txn.columnAt(column); ok {
		idx.Union(&txn.index)
	}

	// go through extra indexes
	for _, e := range extra {
		if idx, ok := txn.columnAt(e); ok {
			idx.Union(&txn.index)
		}
	}
	return txn
}

// WithValue applies a filter predicate over values for a specific properties. It filters
// down the items in the query.
func (txn *Txn) WithValue(column string, predicate func(v interface{}) bool) *Txn {
	if p, ok := txn.columnAt(column); ok {
		txn.index.Filter(func(x uint32) (match bool) {
			if v, ok := p.Value(x); ok {
				match = predicate(v)
			}
			return
		})
	}
	return txn
}

// WithFloat filters down the values based on the specified predicate. The column for
// this filter must be numerical and convertible to float64.
func (txn *Txn) WithFloat(column string, predicate func(v float64) bool) *Txn {
	if p, ok := txn.columnAt(column); ok {
		if n, ok := p.(numerical); ok {
			txn.index.Filter(func(x uint32) (match bool) {
				if v, ok := n.Float64(x); ok {
					match = predicate(v)
				}
				return
			})
		}
	}
	return txn
}

// WithInt filters down the values based on the specified predicate. The column for
// this filter must be numerical and convertible to int64.
func (txn *Txn) WithInt(column string, predicate func(v int64) bool) *Txn {
	if p, ok := txn.columnAt(column); ok {
		if n, ok := p.(numerical); ok {
			txn.index.Filter(func(x uint32) (match bool) {
				if v, ok := n.Int64(x); ok {
					match = predicate(v)
				}
				return
			})
		}
	}
	return txn
}

// WithUint filters down the values based on the specified predicate. The column for
// this filter must be numerical and convertible to uint64.
func (txn *Txn) WithUint(column string, predicate func(v uint64) bool) *Txn {
	if p, ok := txn.columnAt(column); ok {
		if n, ok := p.(numerical); ok {
			txn.index.Filter(func(x uint32) (match bool) {
				if v, ok := n.Uint64(x); ok {
					match = predicate(v)
				}
				return
			})
		}
	}
	return txn
}

// WithString filters down the values based on the specified predicate. The column for
// this filter must be a string.
func (txn *Txn) WithString(column string, predicate func(v string) bool) *Txn {
	return txn.WithValue(column, func(v interface{}) bool {
		return predicate(v.(string))
	})
}

// Count returns the number of objects matching the query
func (txn *Txn) Count() int {
	return int(txn.index.Count())
}

// ReadAt returns a selector for a specified index together with a boolean value that indicates
// whether an element is present at the specified index or not.
func (txn *Txn) ReadAt(index uint32) (Selector, bool) {
	if !txn.index.Contains(index) {
		return Selector{}, false
	}

	return Selector{
		idx: index,
		txn: txn,
	}, true
}

// DeleteAt attempts to delete an item at the specified index for this transaction. If the item
// exists, it marks at as deleted and returns true, otherwise it returns false.
func (txn *Txn) DeleteAt(index uint32) bool {
	if !txn.index.Contains(index) {
		return false
	}

	txn.deletes.Set(index)
	return true
}

// Insert inserts an object at a new index and returns the index for this object. This is
// done transactionally and the object will only be visible after the transaction is committed.
func (txn *Txn) Insert(object Object) uint32 {
	return txn.insert(object, 0)
}

// InsertWithTTL inserts an object at a new index and returns the index for this object. In
// addition, it also sets the time-to-live of an object to the specified time. This is done
// transactionally and the object will only be visible after the transaction is committed.
func (txn *Txn) InsertWithTTL(object Object, ttl time.Duration) uint32 {
	return txn.insert(object, time.Now().Add(ttl).UnixNano())
}

// Insert inserts an object at a new index and returns the index for this object. This is
// done transactionally and the object will only be visible after the transaction is committed.
func (txn *Txn) insert(object Object, expireAt int64) uint32 {
	slot := Cursor{
		Selector: Selector{
			idx: txn.owner.next(),
			txn: txn,
		},
	}

	// Set the insert bit and generate the updates
	txn.inserts.Set(slot.idx)
	for k, v := range object {
		if _, ok := txn.columnAt(k); ok {
			slot.UpdateAt(k, v)
		}
	}

	// Add expiration if specified
	if expireAt != 0 {
		slot.UpdateAt(expireColumn, expireAt)
	}
	return slot.idx
}

// Select iterates over the result set and allows to read any column. While this
// is flexible, it is not the most efficient way, consider Range() as an alternative
// iteration method over a specific column which also supports modification.
func (txn *Txn) Select(fn func(v Selector) bool) {
	txn.index.Range(func(x uint32) bool {
		return fn(Selector{
			idx: x,
			txn: txn,
		})
	})
}

// DeleteIf iterates over the result set and calls the provided funciton on each element. If
// the function returns true, the element at the index will be marked for deletion. The
// actual delete will take place once the transaction is committed.
func (txn *Txn) DeleteIf(fn func(v Selector) bool) {
	txn.index.Range(func(x uint32) bool {
		if fn(Selector{idx: x, txn: txn}) {
			txn.deletes.Set(x)
		}
		return true
	})
}

// DeleteAll marks all of the items currently selected by this transaction for deletion. The
// actual delete will take place once the transaction is committed.
func (txn *Txn) DeleteAll() {
	txn.deletes.Or(txn.index)
}

// Range selects and iterates over a results for a specific column. The cursor provided
// also allows to select other columns, but at a slight performance cost. If the range
// function returns false, it halts the iteration.
func (txn *Txn) Range(column string, fn func(v Cursor) bool) error {
	cur, err := txn.cursorFor(column)
	if err != nil {
		return err
	}

	txn.index.Range(func(x uint32) bool {
		cur.idx = x
		return fn(cur)
	})
	return nil
}

// cursorFor returns a cursor for a specified column
func (txn *Txn) cursorFor(columnName string) (Cursor, error) {
	c, ok := txn.columnAt(columnName)
	if !ok {
		return Cursor{}, fmt.Errorf("column: specified column '%v' does not exist", columnName)
	}

	// Attempt to find the existing update queue index for this column
	updateQueueIndex := -1
	for i, c := range txn.updates {
		if c.name == columnName {
			updateQueueIndex = i
			break
		}
	}

	// Create a new update queue for the selected column
	if updateQueueIndex == -1 {
		updateQueueIndex = len(txn.updates)
		txn.updates = append(txn.updates, updateQueue{
			name:   columnName,
			update: make([]Update, 0, 64),
		})
	}

	// Create a Cursor
	return Cursor{
		column: c,
		update: int16(updateQueueIndex),
		Selector: Selector{
			txn: txn,
		},
	}, nil
}

// Commit commits the transaction by applying all pending updates and deletes to
// the collection. This operation is can be called several times for a transaction
// in order to perform partial commits. If there's no pending updates/deletes, this
// operation will result in a no-op.
func (txn *Txn) Commit() {
	txn.deletePending()
	txn.updatePending()
	txn.insertPending()
}

// Rollback empties the pending update and delete queues and does not apply any of
// the pending updates/deletes. This operation can be called several times for
// a transaction in order to perform partial rollbacks.
func (txn *Txn) Rollback() {
	txn.deletes.Clear()
	txn.inserts.Clear()
	for i := range txn.updates {
		txn.updates[i].update = txn.updates[i].update[:0]
	}
}

// updatePending updates the pending entries that were modified during the query
func (txn *Txn) updatePending() {
	for i, u := range txn.updates {
		if len(u.update) == 0 {
			continue // No updates for this column
		}

		// Get the column that needs to be updated
		columns, exists := txn.owner.cols.LoadWithIndex(u.name)
		if !exists || len(columns) == 0 {
			continue
		}

		// Range through all of the pending updates and apply them to the column
		// and its associated computed columns.
		for _, v := range columns {
			if max, ok := txn.inserts.Max(); ok {
				v.Grow(max)
			}
			v.UpdateMany(u.update)
		}

		// Reset the queue
		txn.updates[i].update = txn.updates[i].update[:0]
	}
}

// deletePending removes all of the entries marked as to be deleted
func (txn *Txn) deletePending() {
	if len(txn.deletes) == 0 {
		return // Nothing to delete
	}

	// Apply a batch delete on all of the columns
	txn.owner.cols.Range(func(column Column) {
		column.DeleteMany(&txn.deletes)
	})

	// Clear the items in the collection and reinitialize the purge list
	txn.owner.lock.Lock()
	txn.owner.fill.AndNot(txn.deletes)
	txn.owner.lock.Unlock()
	txn.deletes.Clear()
}

// insertPending inserts all of the entries marked as to be inserted. This just makes them
// visible by setting the fill list atomically in the collection.
func (txn *Txn) insertPending() {
	if len(txn.inserts) > 0 {
		txn.owner.lock.Lock()
		txn.owner.fill.Or(txn.inserts)
		txn.owner.lock.Unlock()
		txn.inserts.Clear()
	}
}

// --------------------------- Selector ---------------------------

// Selector represents a iteration Selector that supports both retrieval of column values
// for the specified row and modification (update, delete).
type Selector struct {
	idx uint32      // The current index
	txn *Txn        // The (optional) transaction, but one of them is required
	col *Collection // The (optional) collection, but one of them is required
}

// columnAt loads the column based on whether the selector has a transaction or not.
func (cur *Selector) columnAt(column string) (Column, bool) {
	if cur.txn != nil {
		return cur.txn.columnAt(column)
	}

	// Load directly from the collection
	return cur.col.cols.Load(column)
}

// ValueAt reads a value for a current row at a given column.
func (cur *Selector) ValueAt(column string) (out interface{}) {
	if c, ok := cur.columnAt(column); ok {
		out, _ = c.Value(cur.idx)
	}
	return
}

// StringAt reads a string value for a current row at a given column.
func (cur *Selector) StringAt(column string) (out string) {
	if c, ok := cur.columnAt(column); ok {
		if v, ok := c.Value(cur.idx); ok {
			out, _ = v.(string)
		}
	}
	return
}

// FloatAt reads a float64 value for a current row at a given column.
func (cur *Selector) FloatAt(column string) (out float64) {
	if c, ok := cur.columnAt(column); ok {
		if n, ok := c.(numerical); ok {
			out, _ = n.Float64(cur.idx)
		}
	}
	return
}

// IntAt reads an int64 value for a current row at a given column.
func (cur *Selector) IntAt(column string) (out int64) {
	if c, ok := cur.columnAt(column); ok {
		if n, ok := c.(numerical); ok {
			out, _ = n.Int64(cur.idx)
		}
	}
	return
}

// UintAt reads a uint64 value for a current row at a given column.
func (cur *Selector) UintAt(column string) (out uint64) {
	if c, ok := cur.columnAt(column); ok {
		if n, ok := c.(numerical); ok {
			out, _ = n.Uint64(cur.idx)
		}
	}
	return
}

// BoolAt reads a boolean value for a current row at a given column.
func (cur *Selector) BoolAt(column string) bool {
	if c, ok := cur.columnAt(column); ok {
		return c.Contains(cur.idx)
	}
	return false
}

// --------------------------- Cursor ---------------------------

// Cursor represents a iteration Selector that is bound to a specific column.
type Cursor struct {
	Selector
	update int16  // The index of the update queue
	column Column // The selected column
}

// Value reads a value for a current row at a given column.
func (cur *Cursor) Value() (out interface{}) {
	out, _ = cur.column.Value(cur.idx)
	return
}

// String reads a string value for a current row at a given column.
func (cur *Cursor) String() (out string) {
	if v, ok := cur.column.Value(cur.idx); ok {
		out, _ = v.(string)
	}
	return
}

// Float reads a float64 value for a current row at a given column.
func (cur *Cursor) Float() (out float64) {
	if n, ok := cur.column.(numerical); ok {
		out, _ = n.Float64(cur.idx)
	}
	return
}

// Int reads an int64 value for a current row at a given column.
func (cur *Cursor) Int() (out int64) {
	if n, ok := cur.column.(numerical); ok {
		out, _ = n.Int64(cur.idx)
	}
	return
}

// Uint reads a uint64 value for a current row at a given column.
func (cur *Cursor) Uint() (out uint64) {
	if n, ok := cur.column.(numerical); ok {
		out, _ = n.Uint64(cur.idx)
	}
	return
}

// Bool reads a boolean value for a current row at a given column.
func (cur *Cursor) Bool() bool {
	return cur.column.Contains(cur.idx)
}

// --------------------------- Update/Delete ----------------------------

// Delete deletes the current item. The actual operation will be queued and
// executed once the current the transaction completes.
func (cur *Cursor) Delete() {
	cur.txn.deletes.Set(cur.idx)
}

// Update updates a column value for the current item. The actual operation
// will be queued and executed once the current the transaction completes.
func (cur *Cursor) Update(value interface{}) {
	cur.txn.updates[cur.update].update = append(cur.txn.updates[cur.update].update, Update{
		Kind:  UpdatePut,
		Index: cur.idx,
		Value: value,
	})
}

// Add atomically increments/decrements the current value by the specified amount. Note
// that this only works for numerical values and the type of the value must match.
func (cur *Cursor) Add(amount interface{}) {
	cur.txn.updates[cur.update].update = append(cur.txn.updates[cur.update].update, Update{
		Kind:  UpdateAdd,
		Index: cur.idx,
		Value: amount,
	})
}

// UpdateAt updates a specified column value for the current item. The actual operation
// will be queued and executed once the current the transaction completes.
func (cur *Cursor) UpdateAt(column string, value interface{}) {
	for i, c := range cur.txn.updates {
		if c.name == column {
			cur.txn.updates[i].update = append(c.update, Update{
				Kind:  UpdatePut,
				Index: cur.idx,
				Value: value,
			})
			return
		}
	}

	// Create a new update queue
	cur.txn.updates = append(cur.txn.updates, updateQueue{
		name: column,
		update: []Update{{
			Kind:  UpdatePut,
			Index: cur.idx,
			Value: value,
		}},
	})
}

// Add atomically increments/decrements the column value by the specified amount. Note
// that this only works for numerical values and the type of the value must match.
func (cur *Cursor) AddAt(column string, amount interface{}) {
	for i, c := range cur.txn.updates {
		if c.name == column {
			cur.txn.updates[i].update = append(c.update, Update{
				Kind:  UpdateAdd,
				Index: cur.idx,
				Value: amount,
			})
			return
		}
	}

	// Create a new update queue
	cur.txn.updates = append(cur.txn.updates, updateQueue{
		name: column,
		update: []Update{{
			Kind:  UpdateAdd,
			Index: cur.idx,
			Value: amount,
		}},
	})
}
