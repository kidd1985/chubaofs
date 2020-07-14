package metanode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/chubaofs/chubaofs/util"
	"github.com/tecbot/gorocksdb"
	"os"
	"sync"
	"sync/atomic"
)

var readOption = gorocksdb.NewDefaultReadOptions()
var writeOption = gorocksdb.NewDefaultWriteOptions()

func init() {
	readOption.SetFillCache(false)
	writeOption.SetSync(false)
}

type RocksTree struct {
	dir            string
	db             *gorocksdb.DB
	currentApplyID uint64
	sync.Mutex
}

func DefaultRocksTree(dir string) (*RocksTree, error) {
	return NewRocksTree(dir, 2>>32, 4*util.MB)
}

func NewRocksTree(dir string, lruCacheSize int, writeBufferSize int) (*RocksTree, error) {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return nil, err
	}
	tree := &RocksTree{dir: dir}
	basedTableOptions := gorocksdb.NewDefaultBlockBasedTableOptions()
	basedTableOptions.SetBlockCache(gorocksdb.NewLRUCache(lruCacheSize))
	opts := gorocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(basedTableOptions)
	opts.SetCreateIfMissing(true)
	opts.SetWriteBufferSize(writeBufferSize)
	opts.SetMaxWriteBufferNumber(2)
	opts.SetCompression(gorocksdb.NoCompression)
	db, err := gorocksdb.OpenDb(opts, tree.dir)
	if err != nil {
		err = fmt.Errorf("action[openRocksDB],err:%v", err)
		return nil, err
	}
	tree.db = db
	return tree, nil
}
func (r *RocksTree) SetApplyID(id uint64) {
	atomic.StoreUint64(&r.currentApplyID, id)
}

func (r *RocksTree) Flush() error {
	return r.db.Flush(gorocksdb.NewDefaultFlushOptions())
}

var _ Snapshot = &RocksSnapShot{}

type RocksSnapShot struct {
	snap *gorocksdb.Snapshot
	tree *RocksTree
}

func (r *RocksSnapShot) Count(tp TreeType) (uint64, error) {
	var count uint64
	err := r.Range(tp, func(v []byte) (b bool, err error) {
		count += 1
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *RocksSnapShot) Range(tp TreeType, cb func(v []byte) (bool, error)) error {
	return r.tree.RangeWithSnap(r.snap, []byte{byte(tp)}, []byte{byte(tp) + 1}, cb)
}

func (r *RocksSnapShot) Close() {
	r.tree.db.ReleaseSnapshot(r.snap)
}

// This requires global traversal to call carefully
func (r *RocksTree) Count(tp TreeType) uint64 {
	start, end := []byte{byte(tp)}, byte(tp)+1
	var count uint64
	snapshot := r.db.NewSnapshot()
	it := r.Iterator(snapshot)
	defer func() {
		it.Close()
		r.db.ReleaseSnapshot(snapshot)
	}()
	it.Seek(start)
	for ; it.ValidForPrefix(start); it.Next() {
		key := it.Key().Data()
		if key[0] >= end {
			break
		}
		count += 1
	}
	return count
}

func (r *RocksTree) RangeWithSnap(snapshot *gorocksdb.Snapshot, start, end []byte, cb func(v []byte) (bool, error)) error {
	it := r.Iterator(snapshot)
	defer func() {
		it.Close()
	}()
	return r.RangeWithIter(it, start, end, cb)
}

func (r *RocksTree) RangeWithIter(it *gorocksdb.Iterator, start []byte, end []byte, cb func(v []byte) (bool, error)) error {
	it.Seek(start)
	for ; it.ValidForPrefix(start); it.Next() {
		key := it.Key().Data()
		value := it.Value().Data()
		if bytes.Compare(end, key) < 0 {
			break
		}
		if next, err := cb(value); err != nil {
			return err
		} else if !next {
			return nil
		}
	}
	return nil
}

func (r *RocksTree) Range(start, end []byte, cb func(v []byte) (bool, error)) error {
	snapshot := r.db.NewSnapshot()
	defer func() {
		r.db.ReleaseSnapshot(snapshot)
	}()
	return r.RangeWithSnap(snapshot, start, end, cb)
}

func (r *RocksTree) Iterator(snapshot *gorocksdb.Snapshot) *gorocksdb.Iterator {
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	ro.SetSnapshot(snapshot)
	return r.db.NewIterator(ro)
}

// Has checks if the key exists in the btree.
func (r *RocksTree) HasKey(key []byte) (bool, error) {
	bs, err := r.GetBytes(key)
	if err != nil {
		return false, err
	}
	return len(bs) > 0, nil
}

// Has checks if the key exists in the btree.
func (r *RocksTree) GetBytes(key []byte) ([]byte, error) {
	return r.db.GetBytes(readOption, key)
}

// Has checks if the key exists in the btree.
func (r *RocksTree) Put(key []byte, value []byte) error {
	batch := gorocksdb.NewWriteBatch()
	batch.Put(key, value)
	apply := make([]byte, 8)
	binary.BigEndian.PutUint64(apply, r.currentApplyID)
	batch.Put(applyIDKey, apply)
	return r.db.Write(writeOption, batch)
}

// drop the current btree.
func (b *RocksTree) Release() {
	if b.db != nil {
		b.Lock()
		defer b.Unlock()
		b.db.Close()
		b.db = nil
	}
}

var _ InodeTree = &InodeRocks{}
var _ DentryTree = &DentryRocks{}
var _ ExtendTree = &ExtendRocks{}
var _ MultipartTree = &MultipartRocks{}

type InodeRocks struct {
	*RocksTree
}
type DentryRocks struct {
	*RocksTree
}
type ExtendRocks struct {
	*RocksTree
}

type MultipartRocks struct {
	*RocksTree
}

func inodeEncodingKey(ino uint64) []byte {
	buff := bytes.NewBuffer(make([]byte, 9))
	buff.WriteByte(byte(InodeType))
	_ = binary.Write(buff, binary.BigEndian, ino)
	return buff.Bytes()
}

func dentryEncodingKey(parentId uint64, name string) []byte {
	buff := bytes.NewBuffer(make([]byte, 9+len(name)))
	buff.WriteByte(byte(DentryType))
	_ = binary.Write(buff, binary.BigEndian, parentId)
	buff.WriteString(name)
	return buff.Bytes()
}

func extendEncodingKey(ino uint64) []byte {
	buff := bytes.NewBuffer(make([]byte, 9))
	buff.WriteByte(byte(ExtendType))
	_ = binary.Write(buff, binary.BigEndian, ino)
	return buff.Bytes()
}

func multipartEncodingKey(key string, id string) []byte {
	buff := bytes.NewBuffer(make([]byte, len(key)+len(id)))
	buff.WriteByte(byte(MultipartType))
	_ = binary.Write(buff, binary.BigEndian, int(len(key)))
	buff.WriteString(key)
	buff.WriteString(id)
	return buff.Bytes()
}

// count by type
func (b *InodeRocks) Count() uint64 {
	return b.RocksTree.Count(InodeType)
}
func (b *DentryRocks) Count() uint64 {
	return b.RocksTree.Count(DentryType)
}

//Get
func (b *InodeRocks) Get(ino uint64) (*Inode, error) {
	bs, err := b.RocksTree.GetBytes(inodeEncodingKey(ino))
	if err != nil {
		return nil, err
	}
	inode := &Inode{}
	if err := inode.Unmarshal(bs); err != nil {
		return nil, err
	}
	return inode, nil
}
func (b *DentryRocks) Get(ino uint64, name string) (*Dentry, error) {
	bs, err := b.RocksTree.GetBytes(dentryEncodingKey(ino, name))
	if err != nil {
		return nil, err
	}
	dentry := &Dentry{}
	if err := dentry.Unmarshal(bs); err != nil {
		return nil, err
	}
	return dentry, nil
}
func (b *ExtendRocks) Get(ino uint64) (*Extend, error) {
	bs, err := b.RocksTree.GetBytes(extendEncodingKey(ino))
	if err != nil {
		return nil, err
	}
	return NewExtendFromBytes(bs)
}
func (b *MultipartRocks) Get(key, id string) (*Multipart, error) {
	bs, err := b.RocksTree.GetBytes(multipartEncodingKey(key, id))
	if err != nil {
		return nil, err
	}
	return MultipartFromBytes(bs), nil
}

//PUT
func (b *InodeRocks) Put(inode *Inode) error {
	bs, err := inode.Marshal()
	if err != nil {
		return err
	}
	return b.RocksTree.Put(inodeEncodingKey(inode.Inode), bs)
}

func (b *DentryRocks) Put(dentry *Dentry) error {
	bs, err := dentry.Marshal()
	if err != nil {
		return err
	}
	return b.RocksTree.Put(dentryEncodingKey(dentry.ParentId, dentry.Name), bs)
}

func (b *ExtendRocks) Put(extend *Extend) error {
	bs, err := extend.Bytes()
	if err != nil {
		return err
	}
	return b.RocksTree.Put(extendEncodingKey(extend.inode), bs)
}
func (b *MultipartRocks) Put(mutipart *Multipart) error {
	bs, err := mutipart.Bytes()
	if err != nil {
		return err
	}
	return b.RocksTree.Put(multipartEncodingKey(mutipart.key, mutipart.id), bs)
}

//Create if exists , return old, false,   if not  return nil , true
func (b *InodeRocks) Create(inode *Inode) error {

	key := inodeEncodingKey(inode.Inode)
	if has, err := b.HasKey(key); err != nil {
		return err
	} else if has {
		return existsError
	}

	bs, err := inode.Marshal()
	if err != nil {
		return err
	}

	if err = b.RocksTree.Put(key, bs); err != nil {
		return err
	}
	return nil
}

func (b *DentryRocks) Create(dentry *Dentry) error {
	key := dentryEncodingKey(dentry.ParentId, dentry.Name)

	if has, err := b.HasKey(key); err != nil {
		return err
	} else if has {
		return existsError
	}

	bs, err := dentry.Marshal()
	if err != nil {
		return err
	}

	if err = b.RocksTree.Put(key, bs); err != nil {
		return err
	}
	return nil
}
func (b *ExtendRocks) Create(ext *Extend) error {
	key := extendEncodingKey(ext.inode)

	if has, err := b.HasKey(key); err != nil {
		return err
	} else if has {
		return existsError
	}

	bs, err := ext.Bytes()
	if err != nil {
		return err
	}

	if err = b.RocksTree.Put(key, bs); err != nil {
		return err
	}
	return nil
}

func (b *MultipartRocks) Create(mul *Multipart) error {
	key := multipartEncodingKey(mul.key, mul.id)

	if has, err := b.HasKey(key); err != nil {
		return err
	} else if has {
		return existsError
	}

	bs, err := mul.Bytes()
	if err != nil {
		return err
	}

	if err = b.RocksTree.Put(key, bs); err != nil {
		return err
	}
	return nil
}

//Delete
func (b *InodeRocks) Delete(ino uint64) error {
	return b.db.Delete(writeOption, inodeEncodingKey(ino))
}
func (b *DentryRocks) Delete(pid uint64, name string) error {
	return b.db.Delete(writeOption, dentryEncodingKey(pid, name))
}
func (b *ExtendRocks) Delete(ino uint64) error {
	return b.db.Delete(writeOption, extendEncodingKey(ino))
}
func (b *MultipartRocks) Delete(key, id string) error {
	return b.db.Delete(writeOption, multipartEncodingKey(key, id))
}

// Range begin
//Range , if end is nil , it will range all of this type , it range not include end
func (b *InodeRocks) Range(start, end *Inode, cb func(v []byte) (bool, error)) error {
	var (
		startByte []byte
		endByte   []byte
	)
	startByte = inodeEncodingKey(start.Inode)
	if end == nil {
		endByte = []byte{byte(InodeType) + 1}
	} else {
		endByte = inodeEncodingKey(end.Inode)
	}
	return b.RocksTree.Range(startByte, endByte, cb)
}

//Range , if end is nil , it will range all of this type , it range not include end
func (b *DentryRocks) Range(start, end *Dentry, cb func(v []byte) (bool, error)) error {
	var (
		startByte []byte
		endByte   []byte
	)
	startByte = dentryEncodingKey(start.ParentId, start.Name)
	if end == nil {
		endByte = []byte{byte(ExtendType) + 1}
	} else {
		endByte = dentryEncodingKey(end.ParentId, end.Name)
	}
	return b.RocksTree.Range(startByte, endByte, cb)
}

//Range , if end is nil , it will range all of this type , it range not include end
func (b *ExtendRocks) Range(start, end *Extend, cb func(v []byte) (bool, error)) error {
	var (
		startByte []byte
		endByte   []byte
	)
	startByte = extendEncodingKey(start.inode)
	if end == nil {
		endByte = []byte{byte(ExtendType) + 1}
	} else {
		endByte = extendEncodingKey(end.inode)
	}
	return b.RocksTree.Range(startByte, endByte, cb)
}

//Range , if end is nil , it will range all of this type , it range not include end
func (b *MultipartRocks) Range(start, end *Multipart, cb func(v []byte) (bool, error)) error {
	var (
		startByte []byte
		endByte   []byte
	)
	startByte = multipartEncodingKey(start.key, start.id)
	if end == nil {
		endByte = []byte{byte(MultipartType) + 1}
	} else {
		endByte = multipartEncodingKey(end.key, end.id)
	}
	return b.RocksTree.Range(startByte, endByte, cb)
}
