package dynamo

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/cenkalti/backoff"
)

// TODO: chunk into 100 item requests

// Batch stores the names of the hash key and range key
// for creating new batches.
type Batch struct {
	table             Table
	hashKey, rangeKey string
	err               error
}

// Batch creates a new batch with the given hash key name, and range key name if provided.
// For purely Put batches, neither is necessary.
func (table Table) Batch(hashAndRangeKeyName ...string) Batch {
	b := Batch{
		table: table,
	}
	switch len(hashAndRangeKeyName) {
	case 0:
	case 1:
		b.hashKey = hashAndRangeKeyName[0]
	case 2:
		b.hashKey = hashAndRangeKeyName[0]
		b.rangeKey = hashAndRangeKeyName[1]
	default:
		b.err = fmt.Errorf("dynamo: batch: you may only provide the name of a range key and hash key. too many keys.")
	}
	return b
}

// BatchGet is a BatchGetItem operation.
// Note that currently batch gets are limited to 100 items.
type BatchGet struct {
	batch      Batch
	reqs       []*Query
	projection string
	consistent bool
	err        error
}

// Get creates a new batch get item request with the given keys.
//	table.Batch("ID", "Month").
//		Get([]dynamo.Keys{{1, "2015-10"}, {42, "2015-12"}, {42, "1992-02"}}...).
//		All(&results)
// Note that currently batch gets are limited to 100 items.
func (b Batch) Get(keys ...Keyed) *BatchGet {
	bg := &BatchGet{
		batch: b,
		err:   b.err,
	}
	bg.add(keys)
	return bg
}

// And adds more keys to be gotten.
func (bg *BatchGet) And(keys ...Keyed) *BatchGet {
	bg.add(keys)
	return bg
}

func (bg *BatchGet) add(keys []Keyed) {
	for _, key := range keys {
		get := bg.batch.table.Get(bg.batch.hashKey, key.HashKey())
		if rk := key.RangeKey(); bg.batch.rangeKey != "" && rk != nil {
			get.Range(bg.batch.rangeKey, Equal, rk)
			bg.setError(get.err)
		}
		bg.reqs = append(bg.reqs, get)
	}
}

// Consistent will, if on is true, make this batch use a strongly consistent read.
// Reads are eventually consistent by default.
// Strongly consistent reads are more resource-heavy than eventually consistent reads.
func (bg *BatchGet) Consistent(on bool) *BatchGet {
	bg.consistent = on
	return bg
}

// All executes this request and unmarshals all results to out, which must be a pointer to a slice.
func (bg *BatchGet) All(out interface{}) error {
	iter := newBGIter(bg, unmarshalAppend, bg.err)
	for iter.Next(out) {
	}
	return iter.Err()
}

// Iter returns a results iterator for this batch.
func (bg *BatchGet) Iter() Iter {
	return newBGIter(bg, unmarshalItem, bg.err)
}

func (bg *BatchGet) input() *dynamodb.BatchGetItemInput {
	in := &dynamodb.BatchGetItemInput{
		RequestItems: make(map[string]*dynamodb.KeysAndAttributes, 1),
	}

	if bg.projection != "" {
		for _, get := range bg.reqs {
			get.Project(get.projection)
			bg.setError(get.err)
		}
	}

	var kas *dynamodb.KeysAndAttributes
	for _, get := range bg.reqs {
		if kas == nil {
			kas = get.keysAndAttribs()
			continue
		}
		kas.Keys = append(kas.Keys, get.keys())
	}
	if bg.projection != "" {
		kas.ProjectionExpression = &bg.projection
	}
	if bg.consistent {
		kas.ConsistentRead = &bg.consistent
	}
	in.RequestItems[bg.batch.table.Name()] = kas
	return in
}

func (bg *BatchGet) setError(err error) {
	if bg.err == nil {
		bg.err = err
	}
}

// bgIter is the iterator for Batch Get operations
type bgIter struct {
	bg        *BatchGet
	input     *dynamodb.BatchGetItemInput
	output    *dynamodb.BatchGetItemOutput
	err       error
	idx       int
	backoff   *backoff.ExponentialBackOff
	unmarshal unmarshalFunc
}

func newBGIter(bg *BatchGet, fn unmarshalFunc, err error) *bgIter {
	iter := &bgIter{
		bg:        bg,
		err:       err,
		backoff:   backoff.NewExponentialBackOff(),
		unmarshal: fn,
	}
	iter.backoff.MaxElapsedTime = 0
	return iter
}

// Next tries to unmarshal the next result into out.
// Returns false when it is complete or if it runs into an error.
func (itr *bgIter) Next(out interface{}) bool {
	// stop if we have an error
	if itr.err != nil {
		return false
	}

	tableName := itr.bg.batch.table.Name()

	// can we use results we already have?
	if itr.output != nil && itr.idx < len(itr.output.Responses[tableName]) {
		items := itr.output.Responses[tableName]
		item := items[itr.idx]
		itr.err = itr.unmarshal(item, out)
		itr.idx++
		return itr.err == nil
	}

	// new bg
	if itr.input == nil {
		itr.input = itr.bg.input()
	}

	if itr.output != nil && itr.idx >= len(itr.output.Responses[tableName]) {
		// have we exhausted all results?
		if len(itr.output.UnprocessedKeys) == 0 {
			return false
		}

		// no, prepare next request and reset index
		itr.input.RequestItems = itr.output.UnprocessedKeys
		itr.idx = 0
		// we need to sleep here a bit as per the official docs
		time.Sleep(itr.backoff.NextBackOff())
	}

	itr.err = retry(func() error {
		var err error
		itr.output, err = itr.bg.batch.table.db.client.BatchGetItem(itr.input)
		return err
	})

	items := itr.output.Responses[tableName]
	if itr.err != nil || len(items) == 0 {
		if itr.idx == 0 {
			itr.err = ErrNotFound
		}
		return false
	}
	itr.err = itr.unmarshal(items[itr.idx], out)
	itr.idx++
	return itr.err == nil
}

// Err returns the error encountered, if any.
// You should check this after Next is finished.
func (itr *bgIter) Err() error {
	return itr.err
}
