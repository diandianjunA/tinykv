package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(_ context.Context, req *kvrpcpb.RawGetRequest) (resp *kvrpcpb.RawGetResponse, respErr error) {
	// Your Code Here (1).
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		return nil, err
	}
	val, err := reader.GetCF(req.GetCf(), req.GetKey())
	reader.Close()
	resp = new(kvrpcpb.RawGetResponse)
	resp.Value = val
	if val == nil {
		resp.NotFound = true
		return resp, nil
	} else {
		resp.NotFound = false
		return resp, err
	}
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be modified
	put := new(storage.Put)
	put.Key = req.GetKey()
	put.Value = req.GetValue()
	put.Cf = req.GetCf()
	modify := new(storage.Modify)
	modify.Data = *put
	modifies := make([]storage.Modify, 0)
	modifies = append(modifies, *modify)
	err := server.storage.Write(req.GetContext(), modifies)
	if err != nil {
		return nil, err
	}
	resp := new(kvrpcpb.RawPutResponse)
	return resp, nil
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be deleted
	delete := new(storage.Delete)
	delete.Key = req.GetKey()
	delete.Cf = req.GetCf()
	modify := new(storage.Modify)
	modify.Data = *delete
	modifies := make([]storage.Modify, 0)
	modifies = append(modifies, *modify)
	err := server.storage.Write(req.GetContext(), modifies)
	if err != nil {
		return nil, err
	}
	resp := new(kvrpcpb.RawDeleteResponse)
	return resp, nil
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (resp *kvrpcpb.RawScanResponse, respErr error) {
	// Your Code Here (1).
	// Hint: Consider using reader.IterCF
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		return nil, err
	}
	iter := reader.IterCF(req.GetCf())
	iter.Seek(req.StartKey)
	resp = new(kvrpcpb.RawScanResponse)
	var i uint32 = 0
	for ; i < req.Limit && iter.Valid(); i++ {
		pair := new(kvrpcpb.KvPair)
		pair.Key = iter.Item().Key()
		value, err := iter.Item().Value()
		if err != nil {
			return nil, err
		}
		pair.Value = value
		resp.Kvs = append(resp.Kvs, pair)
		iter.Next()
	}
	iter.Close()
	reader.Close()
	return resp, nil
}
