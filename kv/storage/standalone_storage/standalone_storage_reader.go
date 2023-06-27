package standalone_storage

import (
	"github.com/Connor1996/badger"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
)

type StandaloneStorageReader struct {
	txn *badger.Txn
}

func NewStandaloneStorageReader(db *badger.DB) *StandaloneStorageReader {
	s := new(StandaloneStorageReader)
	s.txn = db.NewTransaction(false)
	return s
}

func (s *StandaloneStorageReader) GetCF(cf string, key []byte) ([]byte, error) {
	val, err := engine_util.GetCFFromTxn(s.txn, cf, key)
	if val == nil {
		return val, nil
	} else {
		return val, err
	}
}

func (s *StandaloneStorageReader) IterCF(cf string) engine_util.DBIterator {
	iter := engine_util.NewCFIterator(cf, s.txn)
	return iter
}

func (s *StandaloneStorageReader) Close() {
	s.txn.Discard()
}
