// Copyright (c) 2023, donnie <donnie4w@gmail.com>
// All rights reserved.
// Use of t source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// github.com/donnie4w/wfs
package stroge

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	. "github.com/donnie4w/gofer/hashmap"
	"github.com/donnie4w/gofer/lock"
	. "github.com/donnie4w/gofer/mmap"
	goutil "github.com/donnie4w/gofer/util"
	"github.com/donnie4w/simplelog/logging"
	"github.com/donnie4w/wfs/stub"
	"github.com/donnie4w/wfs/sys"
	"github.com/donnie4w/wfs/util"
	lru "github.com/hashicorp/golang-lru/v2"
)

var serve = &servie{}

type servie struct{}

func (t *servie) Serve() (err error) {
	initStore()
	return
}

func (t *servie) Close() (err error) {
	stopstat = true
	<-time.After(2 * time.Second)
	for _, ldb := range dbMap {
		ldb.Close()
	}
	dataEg.mm.Range(func(k uint64, v *Mmap) bool {
		v.Unmap()
		return true
	})
	return
}

var wfsdb *ldb
var stopstat bool
var seq int64
var count int64
var defragStat = false
var defragfile *os.File
var unmountmap = &sync.Map{}
var defragmap = &sync.Map{}

func init() {
	sys.Serve.Put(1, serve)
	sys.AppendData = fe.append
	sys.GetData = fe.getData
	sys.DelData = fe.delData
	sys.Add = fe.add
	sys.Del = fe.del
	sys.Count = fe.count
	sys.Seq = fe.seq
	sys.SearchLike = fe.findLike
	sys.SearchLimit = fe.findLimit
	sys.FragAnalysis = fe.fragAnalysis
	sys.Defrag = fe.defragAndCover
	sys.Import = importData
	sys.Export = exportData
	sys.Modify = fe.modify
}

func initStore() (err error) {
	if wfsdb, err = New(sys.WFSDATA + "/wfsdb"); err != nil {
		fmt.Println("init error:" + err.Error())
		os.Exit(1)
	}
	var wfsCurrent string
	if v, err := wfsdb.Get(CURRENT); err == nil && v != nil {
		wfsCurrent = string(v)
	}
	if v, err := wfsdb.Get(SEQ); err == nil && v != nil {
		seq = goutil.BytesToInt64(v)
	}
	if v, err := wfsdb.Get(COUNT); err == nil && v != nil {
		count = goutil.BytesToInt64(v)
	}
	openFileEg(wfsCurrent)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	freemem := uint64(sys.Memlimit*sys.MB) - m.TotalAlloc
	length := freemem / 128
	if length > 1<<20 {
		length = 1 << 20
	} else if length < 1<<10 {
		length = 1 << 10
	}
	catch, _ = lru.New[string, []byte](int(length))
	initDefrag()
	return
}

func initDefrag() {
	filepath.WalkDir(sys.WFSDATA+"/wfsfile", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if strings.Contains(d.Name(), "_") {
				name := d.Name()
				name = name[:strings.Index(name, "_")]
				if id, ok := strToInt(name); ok && util.CheckNodeId(int64(id)) {
					os.Remove(path)
					fe.defragAndCover(name)
				}
			}
		}
		return nil
	})
}

var numlock = lock.NewNumLock(1 << 9)

var dataEg = &dataHandler{mm: NewMap[uint64, *Mmap]()}

var referMap = NewLimitMap[int64, *int32](1 << 15)

type dataHandler struct {
	mm *Map[uint64, *Mmap]
}

func (t *dataHandler) openMMap(node string) (_r bool) {
	if id, b := strToInt(node); b {
		lockid := goutil.CRC64(append(OPENMMAPLOCK_, goutil.Int64ToBytes(int64(id))...))
		numlock.Lock(int64(lockid))
		defer numlock.Unlock(int64(lockid))
		if !t.mm.Has(id) {
			path := getpathBynode(node)
			if goutil.IsFileExist(path) {
				if f, err := util.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666); err == nil {
					if n, err := NewMMAP(f, 0); err == nil {
						t.mm.Put(id, n)
						_r = true
					}
				}
			}
		}
		_r = t.mm.Has(id)
	}
	return
}

func (t *dataHandler) getData(node string, offset int64, size int64) (bs []byte, ok bool) {
	if defragStat {
		if _, b := defragmap.Load(node); b {
			if bs, b := t.getDataByfile(offset, size); b {
				return bs, b
			}
		}
	}
	if id, b := strToInt(node); b {
		if _, ok := unmountmap.Load(id); !ok && !t.mm.Has(id) {
			t.openMMap(node)
		}
		if m, b := t.mm.Get(id); b {
			if offset+size <= int64(len(m.Bytes())) {
				bs, ok = m.Bytes()[offset:offset+size], true
			}
		}
	}
	return
}

func (t *dataHandler) getDataByfile(offset int64, size int64) (bs []byte, ok bool) {
	defer util.Recover()
	if defragfile != nil {
		bs = make([]byte, size)
		if n, err := defragfile.ReadAt(bs, offset); err == nil && n == int(size) {
			return bs, true
		} else {
			return nil, false
		}
	}
	return
}

func (t *dataHandler) reSetMMap(node string, m *Mmap) {
	if id, b := strToInt(node); b {
		lockid := goutil.CRC64(append(RESETMMAPLOCK_, goutil.Int64ToBytes(int64(id))...))
		numlock.Lock(int64(lockid))
		defer numlock.Unlock(int64(lockid))
		if oldm, b := t.mm.Get(id); b {
			oldm.UnmapAndCloseFile()
		}
		t.mm.Put(id, m)
	}
}

func (t *dataHandler) unMmap(node string) {
	if id, b := strToInt(node); b {
		t.unMmapById(id)
	}
}

func (t *dataHandler) unMmapById(id uint64) {
	if oldm, b := t.mm.Get(id); b {
		oldm.UnmapAndCloseFile()
	}
	t.mm.Del(id)
}

var fe = &fileEg{mux: &sync.Mutex{}}

type fileEg struct {
	handler *fileHandler
	mux     *sync.Mutex
}

func openFileEg(node string) {
	fe.handler, _ = initFileHandler(node)
}

func (t *fileEg) add(key, value []byte) error {
	return wfsdb.Put(key, value)
}

func (t *fileEg) del(key []byte) error {
	return wfsdb.Del(key)
}

func (t *fileEg) count() int64 {
	return count
}
func (t *fileEg) seq() int64 {
	return seq
}

func (t *fileEg) findLike(pathprx string) (_r []*sys.PathBean) {
	defer util.Recover()
	pathpre := append(PATH_PRE, []byte(pathprx)...)
	if bys, err := wfsdb.GetValuesPrefix(pathpre); err == nil {
		_r = make([]*sys.PathBean, 0)
		for _, v := range bys {
			i := goutil.BytesToInt64(v)
			pathseqkey := append(PATH_SEQ, goutil.Int64ToBytes(i)...)
			if wpbbs, err := wfsdb.Get(pathseqkey); err == nil {
				wpb := bytesToWfsPathBean(wpbbs)
				if bs := t.getData(*wpb.Path); bs != nil {
					pb := &sys.PathBean{Id: i, Path: *wpb.Path, Body: bs, Timestramp: *wpb.Timestramp}
					_r = append(_r, pb)
				} else {
					t.delData(*wpb.Path)
				}
			}
		}
	}
	return
}

func (t *fileEg) findLimit(start, limit int64) (_r []*sys.PathBean) {
	if start-limit > seq {
		return
	}
	defer util.Recover()
	var count int64
	_r = make([]*sys.PathBean, 0)
	for i := start; i > 0 && count < limit; i-- {
		pathseqkey := append(PATH_SEQ, goutil.Int64ToBytes(i)...)
		if wpbbs, err := wfsdb.Get(pathseqkey); err == nil {
			wpb := bytesToWfsPathBean(wpbbs)
			if bs := t.getData(*wpb.Path); bs != nil {
				pb := &sys.PathBean{Id: i, Path: *wpb.Path, Body: bs, Timestramp: *wpb.Timestramp}
				_r = append(_r, pb)
				count++
			} else {
				t.delData(*wpb.Path)
			}
		} else if i > seq {
			count++
		}
	}
	return
}

func (t *fileEg) append(path string, bs []byte, compressType int32) (id int64, _r sys.ERROR) {
	if stopstat {
		return id, sys.ERR_STOPSERVICE
	}
	if path == "" || bs == nil || len(bs) == 0 {
		return id, sys.ERR_PARAMS
	}
	if len(bs) > int(sys.FileSize) {
		return id, sys.ERR_OVERSIZE
	}
	defer util.Recover()
	node := t.handler.Node
	var nf bool
	if nf, _r = t.handler.append(path, bs, compressType); _r != nil && _r.Equal(sys.ERR_FILEAPPEND) {
		if err := t.next(node); err == nil {
			nf, _r = t.handler.append(path, bs, compressType)
		} else {
			return id, sys.ERR_FILECREATE
		}
	}

	if nf && _r == nil && sys.Mode == 1 {
		m := make(map[*[]byte][]byte, 0)
		id = atomic.AddInt64(&seq, 1)

		m[&SEQ] = goutil.Int64ToBytes(seq)
		pathpre := append(PATH_PRE, []byte(path)...)
		m[&pathpre] = goutil.Int64ToBytes(id)

		pathseqkey := append(PATH_SEQ, goutil.Int64ToBytes(id)...)
		t := time.Now().UnixNano()
		wpb := &stub.WfsPathBean{Path: &path, Timestramp: &t}
		m[&pathseqkey] = wfsPathBeanToBytes(wpb)

		wfsdb.BatchPut(m)
	}
	return
}

func (t *fileEg) getData(path string) (_r []byte) {
	if stopstat {
		return nil
	}
	defer util.Recover()
	fid := goutil.CRC64([]byte(path))
	fidbs := goutil.Int64ToBytes(int64(fid))
	if v, err := catchGet(fidbs); err == nil {
		if v, err = catchGet(v); err == nil {
			wfb := bytesToWfsFileBean(v)
			if bs, b := dataEg.getData(*wfb.Storenode, *wfb.Offset, 12+*wfb.Size); b {
				_r = praseUncompress(bs[12:], *wfb.CompressType)
			}
		}
	}
	return
}

func (t *fileEg) delData(path string) (_r sys.ERROR) {
	if stopstat {
		return sys.ERR_STOPSERVICE
	}
	defer util.Recover()
	fid := goutil.CRC64([]byte(path))
	fidbs := goutil.Int64ToBytes(int64(fid))
	batchmap := make(map[*[]byte][]byte, 0)
	dels := [][]byte{fidbs}
	if oldBidBs, err := wfsdb.Get(fidbs); err == nil && oldBidBs != nil {
		if oldWffsBs, err := wfsdb.Get(oldBidBs); err == nil && oldWffsBs != nil {
			oldwffs := bytesToWfsFileBean(oldWffsBs)
			nid, _ := strToInt(*oldwffs.Storenode)
			nidbs := goutil.Int64ToBytes(int64(nid))
			*oldwffs.Refercount -= 1
			if *oldwffs.Refercount <= 0 {
				if nodebs, err := wfsdb.Get(nidbs); err == nil && nodebs != nil {
					wnb := bytesToWfsNodeBean(nodebs)
					*wnb.Rmsize = *wnb.Rmsize + *oldwffs.Size
					batchmap[&nidbs] = wfsNodeBeanToBytes(wnb)
					dels = append(dels, oldBidBs)
				}
			} else {
				batchmap[&oldBidBs] = wfsFileBeanToBytes(oldwffs)
			}
		}
		if sys.Mode == 1 {
			pathpre := append(PATH_PRE, []byte(path)...)
			if v, err := wfsdb.Get(pathpre); err == nil {
				dels = append(dels, pathpre)
				dels = append(dels, append(PATH_SEQ, v...))
			}
		}
		batchmap[&COUNT] = goutil.Int64ToBytes(atomic.AddInt64(&count, -1))
	} else {
		return sys.ERR_NOTEXSIT
	}

	if err := wfsdb.Batch(batchmap, dels); err != nil {
		return sys.ERR_UNDEFINED
	} else {
		catchDel(fidbs)
	}
	return
}

func (t *fileEg) modify(path, newpath string) (err sys.ERROR) {
	if stopstat {
		return sys.ERR_STOPSERVICE
	}
	if path == "" || newpath == "" || path == newpath {
		return sys.ERR_PARAMS
	}
	am := make(map[*[]byte][]byte, 0)
	dm := make([][]byte, 0)
	fid := goutil.CRC64([]byte(path))
	fidbs := goutil.Int64ToBytes(int64(fid))
	dm = append(dm, fidbs)
	newfid := goutil.CRC64([]byte(newpath))
	newfidbs := goutil.Int64ToBytes(int64(newfid))
	pathpre := append(PATH_PRE, []byte(path)...)
	dm = append(dm, pathpre)
	if v, err := wfsdb.Get(pathpre); err == nil {
		newpathpre := append(PATH_PRE, []byte(newpath)...)
		am[&newpathpre] = v

		i := goutil.BytesToInt64(v)
		pathseqkey := append(PATH_SEQ, goutil.Int64ToBytes(i)...)
		if v, err := wfsdb.Get(pathseqkey); err == nil {
			wpb := bytesToWfsPathBean(v)
			wpb.Path = &newpath
			am[&pathseqkey] = wfsPathBeanToBytes(wpb)
		}
	} else {
		return sys.ERR_NOTEXSIT
	}
	if oldBidBs, err := wfsdb.Get(fidbs); err == nil && oldBidBs != nil {
		am[&newfidbs] = oldBidBs
	} else {
		return sys.ERR_NOTEXSIT
	}
	if _, err := wfsdb.Get(newfidbs); err != nil && len(am) > 0 && len(dm) > 0 {
		if wfsdb.Batch(am, dm) == nil {
			catchDel(fidbs)
			catchDel(pathpre)
		}
	} else {
		return sys.ERR_NEWPATHEXIST
	}
	return
}

func (t *fileEg) next(node string) (err error) {
	t.mux.Lock()
	defer t.mux.Unlock()
	if node == t.handler.Node {
		t.handler, err = initFileHandler("")
	}
	return
}

// Deprecated
func (t *fileEg) defrag(node string) (err sys.ERROR) {
	if stopstat {
		return sys.ERR_STOPSERVICE
	}
	defragStat = true
	defer func() {
		if e := recover(); e != nil {
			err = sys.ERR_UNDEFINED
		}
		defragStat = false
		defragmap.Delete(node)
	}()
	if v, err := wfsdb.Get(CURRENT); err == nil && v != nil {
		if string(v) == node {
			return sys.ERR_DEFRAG_FORBID
		}
	}

	nid, b := strToInt(node)
	if !b {
		return sys.ERR_NOTEXSIT
	}
	dataEg.openMMap(node)
	mm, b := dataEg.mm.Get(nid)
	if !b {
		return sys.ERR_NOTEXSIT
	}

	newnid := util.CreateNodeId()
	newnode := intToStr(uint64(newnid))
	newnidbs := goutil.Int64ToBytes(newnid)
	newnodepath := getpathBynode(newnode)
	if err := os.MkdirAll(filepath.Dir(newnodepath), 0777); err != nil {
		return sys.ERR_UNDEFINED
	}
	if f, err := util.OpenFile(newnodepath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666); err == nil {
		defragfile = f
		defer f.Close()
		wl := new(int64)
		if err = defragFB(mm.Bytes(), f, 0, &newnode, wl, new(int32)); err == nil {
			var size int64
			if fs, _ := f.Stat(); fs != nil {
				size = fs.Size()
			} else {
				size = *wl
			}
			oldOffsBs := append(ENDOFFSET_, goutil.Int64ToBytes(int64(nid))...)
			newOffset := append(ENDOFFSET_, goutil.Int64ToBytes(int64(newnid))...)
			newbat := make(map[*[]byte][]byte, 0)
			if *wl == 0 && size == 0 {
				f.Close()
				os.Remove(newnodepath)
				f = nil
			} else {
				newbat[&newOffset] = goutil.Int64ToBytes(size)
				newbat[&newnidbs] = wfsNodeBeanToBytes(&stub.WfsNodeBean{Rmsize: new(int64)})
			}
			wfsdb.Batch(newbat, [][]byte{oldOffsBs})
			dataEg.unMmap(node)
			if err := os.Remove(getpathBynode(node)); err != nil {
				logging.Error(err)
			}
			dataEg.unMmap(newnode)
			dataEg.openMMap(newnode)

		} else {
			if fs, e := f.Stat(); e == nil {
				if fs.Size() == 0 && *wl == 0 {
					f.Close()
					os.Remove(newnodepath)
					f = nil
				}
			}
			return sys.ERR_UNDEFINED
		}
	}
	return
}

func (t *fileEg) defragAndCover(node string) (err sys.ERROR) {
	if stopstat {
		return sys.ERR_STOPSERVICE
	}
	defragStat = true
	defer func() {
		if e := recover(); e != nil {
			err = sys.ERR_UNDEFINED
		}
		defragStat = false
		defragmap.Delete(node)
	}()
	if v, err := wfsdb.Get(CURRENT); err == nil && v != nil {
		if string(v) == node {
			return sys.ERR_DEFRAG_FORBID
		}
	}
	nid, b := strToInt(node)
	if !b {
		return sys.ERR_NOTEXSIT
	}
	dataEg.openMMap(node)
	mm, b := dataEg.mm.Get(nid)
	if !b {
		return sys.ERR_NOTEXSIT
	}
	nidbs := goutil.Int64ToBytes(int64(nid))
	nodepath := getpathBynode(node)

	newnode := fmt.Sprint(node, "_", util.CreateNodeId())
	newnodepath := getpathBynode(newnode)
	if err := os.MkdirAll(filepath.Dir(newnodepath), 0777); err != nil {
		return sys.ERR_UNDEFINED
	}
	if f, err := util.OpenFile(newnodepath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666); err == nil {
		defragfile = f
		defer f.Close()
		wl := new(int64)
		defragmap.Store(node, newnode)
		if err = defragFB(mm.Bytes(), f, 0, &node, wl, new(int32)); err == nil {
			var size int64
			if fs, _ := f.Stat(); fs != nil {
				size = fs.Size()
			} else {
				size = *wl
			}
			if *wl == 0 && size == 0 {
				f.Close()
				os.Remove(newnodepath)
				f = nil
				return sys.ERR_UNDEFINED
			} else {
				offsBs := append(ENDOFFSET_, goutil.Int64ToBytes(int64(nid))...)
				newbat := make(map[*[]byte][]byte)
				newbat[&offsBs] = goutil.Int64ToBytes(size)
				newbat[&nidbs] = wfsNodeBeanToBytes(&stub.WfsNodeBean{Rmsize: new(int64)})
				wfsdb.BatchPut(newbat)
			}
			unmountmap.Store(nid, byte(0))
			defer unmountmap.Delete(nid)
			dataEg.unMmap(node)
			dataEg.unMmap(newnode)
			f.Close()
			if os.Rename(newnodepath, nodepath) == nil {
				dataEg.openMMap(node)
			}
		} else {
			if fs, e := f.Stat(); e == nil {
				if fs.Size() == 0 && *wl == 0 {
					f.Close()
					os.Remove(newnodepath)
					f = nil
				}
			}
			return sys.ERR_UNDEFINED
		}
	}
	return
}

func getpathBynode(node string) string {
	return sys.WFSDATA + "/wfsfile/" + node
}

func defragFB(bs []byte, f *os.File, offset int64, node *string, wl *int64, rl *int32) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = errors.New(fmt.Sprint(e))
		}
	}()
	if len(bs) > 12 {
		if v, e := wfsdb.Get(bs[:8]); e == nil && v != nil {
			wfb := bytesToWfsFileBean(v)
			wfb.Storenode = node
			*wfb.Offset = offset
			size := goutil.BytesToInt32(bs[8:12])
			if n, e := f.Write(bs[:12+size]); e == nil {
				atomic.AddInt64(wl, int64(n))
				catchDel(bs[:8])
				if err = wfsdb.Put(bs[:8], wfsFileBeanToBytes(wfb)); err == nil {
					return defragFB(bs[n:], f, offset+int64(n), node, wl, rl)
				}
			} else {
				return e
			}
		} else {
			if size := goutil.BytesToInt32(bs[8:12]); size > 0 || *rl < 4 {
				if size == 0 {
					atomic.AddInt32(rl, 1)
				}
				err = defragFB(bs[12+size:], f, offset, node, wl, rl)
			}
		}
	}
	return
}

func (t *fileEg) fragAnalysis(node string) (fb *sys.FragBean, err sys.ERROR) {
	if stopstat {
		return nil, sys.ERR_STOPSERVICE
	}
	defer func() {
		if e := recover(); e != nil {
			err = sys.ERR_UNDEFINED
		}
	}()

	if v, err := wfsdb.Get(CURRENT); err == nil && v != nil {
		if string(v) == node {
			return nil, sys.ERR_DEFRAG_FORBID
		}
	}
	fb = &sys.FragBean{Node: node}
	nid, _ := strToInt(node)
	nidbs := goutil.Int64ToBytes(int64(nid))
	ofsBs := append(ENDOFFSET_, nidbs...)
	if v, err := wfsdb.Get(ofsBs); err == nil {
		fb.ActualSize = goutil.BytesToInt64(v)
	}
	if nodebs, err := wfsdb.Get(nidbs); err == nil && nodebs != nil {
		wnb := bytesToWfsNodeBean(nodebs)
		fb.RmSize = *wnb.Rmsize
	}
	if f, err := os.Stat(getpathBynode(node)); err == nil {
		fb.FileSize = f.Size()
	}
	return
}

type fileHandler struct {
	mm     *Mmap
	Node   string
	length int64
}

func initFileHandler(node string) (fh *fileHandler, err error) {
	fine := false
	if node == "" {
		if fh, err = newFileHandler(); err == nil {
			return
		} else {
			sys.FmtLog(err)
		}
	} else {
		nid, _ := strToInt(node)
		nidbs := goutil.Int64ToBytes(int64(nid))

		if endoffsetBs, err := wfsdb.Get(append(ENDOFFSET_, nidbs...)); err == nil {
			if endOffset := goutil.BytesToInt64(endoffsetBs); endOffset < sys.FileSize {
				nodepath := getpathBynode(node)
				if goutil.IsFileExist(nodepath) {
					if f, err := util.OpenFile(nodepath, os.O_CREATE|os.O_RDWR, 0666); err == nil {
						if mm, err := NewMMAP(f, endOffset); err == nil {
							dataEg.reSetMMap(node, mm)
							fh = &fileHandler{mm: mm, Node: node, length: endOffset}
							fine = true
						}
					}
				}
			}
		}

		if !fine {
			initFileHandler("")
			return
		}
	}
	if !fine {
		return initFileHandler("")
	}
	return
}

func newFileHandler() (fh *fileHandler, err error) {
	nid := util.CreateNodeId()
	node := intToStr(uint64(nid))
	nidbs := goutil.Int64ToBytes(nid)
	var f *os.File
	nodepath := getpathBynode(node)
	if err = os.MkdirAll(filepath.Dir(nodepath), 0777); err != nil {
		return
	}
	if f, err = util.OpenFile(nodepath, os.O_CREATE|os.O_RDWR, 0666); err == nil {
		if err = f.Truncate(sys.FileSize); err == nil {
			var mm *Mmap
			if mm, err = NewMMAP(f, 0); err == nil {
				dataEg.reSetMMap(node, mm)
				fh = &fileHandler{mm: mm, Node: node}

				fmap := make(map[*[]byte][]byte, 0)
				ofsBs := append(ENDOFFSET_, nidbs...)
				fmap[&ofsBs] = []byte{0}
				fmap[&nidbs] = wfsNodeBeanToBytes(&stub.WfsNodeBean{Rmsize: new(int64)})
				fmap[&CURRENT] = []byte(node)
				err = wfsdb.BatchPut(fmap)
			}
		}
	}
	return
}

func (t *fileHandler) append(path string, bs []byte, compressType int32) (nf bool, _r sys.ERROR) {
	if path != "" && bs != nil && len(bs) > 0 {
		fid := goutil.CRC64([]byte(path))
		fidBs := goutil.Int64ToBytes(int64(fid))

		lockid := goutil.CRC64(append(APPENDLOCK_, fidBs...))
		numlock.Lock(int64(lockid))
		defer numlock.Unlock(int64(lockid))

		bId := goutil.CRC64(bs)
		bidBs := goutil.Int64ToBytes(int64(bId))

		nid, _ := strToInt(t.Node)
		nidbs := goutil.Int64ToBytes(int64(nid))

		var wfbbs []byte

		if v, err := wfsdb.Get(bidBs); err != nil || v == nil {
			storeBytes := praseCompress(bs, compressType)
			if atomic.AddInt64(&t.length, int64(len(storeBytes)+12)) < sys.FileSize {
				size, refer := int64(len(storeBytes)), new(int32)
				*refer = 1

				if r, ok := referMap.LoadOrStore(int64(bId), refer); ok {
					refer = r
					atomic.AddInt32(refer, 1)
				}

				wfb := &stub.WfsFileBean{Storenode: &t.Node, Size: &size, CompressType: &compressType, Refercount: refer}

				fmap := make(map[*[]byte][]byte, 0)

				bs := append(bidBs, goutil.Int32ToBytes(int32(size))...)
				if !sys.SYNC {
					if n, err := t.mm.Append(append(bs, storeBytes...)); err == nil {
						wfb.Offset = &n
					} else {
						return nf, sys.ERR_FILEAPPEND
					}
				} else {
					if n, err := t.mm.AppendSync(append(bs, storeBytes...)); err == nil {
						wfb.Offset = &n
					} else {
						return nf, sys.ERR_FILEAPPEND
					}
				}

				wfbbytes := wfsFileBeanToBytes(wfb)
				fmap[&bidBs] = wfbbytes

				ofsBs := append(ENDOFFSET_, nidbs...)
				fmap[&ofsBs] = goutil.Int64ToBytes(t.length)

				if err := wfsdb.BatchPut(fmap); err != nil {
					return nf, sys.ERR_UNDEFINED
				} else {
					catchPut(bidBs, wfbbytes)
				}

			} else {
				return nf, sys.ERR_FILEAPPEND
			}
		} else {
			wfbbs = v
		}
		batchmap := make(map[*[]byte][]byte, 0)
		var dels [][]byte
		if oldBidBs, err := wfsdb.Get(fidBs); err == nil && oldBidBs != nil {
			if bytes.Equal(oldBidBs, bidBs) {
				return nf, sys.ERR_EXSIT
			}
			if oldWffsBs, err := wfsdb.Get(oldBidBs); err == nil && oldWffsBs != nil {
				oldwffs := bytesToWfsFileBean(oldWffsBs)
				*oldwffs.Refercount -= 1
				if *oldwffs.Refercount <= 0 {
					if nodebs, err := wfsdb.Get(nidbs); err == nil && nodebs != nil {
						wnb := bytesToWfsNodeBean(nodebs)
						*wnb.Rmsize = *wnb.Rmsize + *oldwffs.Size
						batchmap[&nidbs] = wfsNodeBeanToBytes(wnb)
						dels = [][]byte{oldBidBs}
					}
				} else {
					batchmap[&oldBidBs] = wfsFileBeanToBytes(oldwffs)
				}
			}
		} else {
			nf = true
			batchmap[&COUNT] = goutil.Int64ToBytes(atomic.AddInt64(&count, 1))
		}

		if wfbbs != nil {
			wfb := bytesToWfsFileBean(wfbbs)
			if r, ok := referMap.LoadOrStore(int64(bId), wfb.Refercount); ok {
				wfb.Refercount = r
			}
			atomic.AddInt32(wfb.Refercount, 1)
			batchmap[&bidBs] = wfsFileBeanToBytes(wfb)
		}

		batchmap[&fidBs] = bidBs
		if err := wfsdb.Batch(batchmap, dels); err != nil {
			return nf, sys.ERR_UNDEFINED
		} else {
			catchPut(fidBs, bidBs)
		}
	} else {
		return nf, sys.ERR_PARAMS
	}
	return
}

func exportData(streamfunc func(bean *stub.SnapshotBean) bool) (err error) {
	defer util.Recover()
	return wfsdb.SnapshotToStream(nil, streamfunc)
}

func importData(bean *stub.SnapshotBean, cover bool) (err error) {
	defer util.Recover()
	if bean == nil {
		return errors.New("data is nil")
	}
	if bytes.Equal(bean.Key, CURRENT) || bytes.Equal(bean.Key, COUNT) || bytes.Equal(bean.Key, SEQ) {
		return
	}
	if len(bean.Key) == 17 && bytes.Equal(bean.Key[:9], PATH_SEQ) {
		wppb := bytesToWfsPathBean(bean.Value)
		path := *wppb.Path
		pathpre := append(PATH_PRE, []byte(path)...)
		am := make(map[*[]byte][]byte, 0)
		dm := make([][]byte, 0)
		if v, e := wfsdb.Get(pathpre); e == nil {
			if !cover {
				return
			}
			oldpathseqkey := append(PATH_SEQ, v...)
			dm = append(dm, oldpathseqkey)
		} else {
			am[&COUNT] = goutil.Int64ToBytes(atomic.AddInt64(&count, 1))
		}
		id := atomic.AddInt64(&seq, 1)
		am[&SEQ] = goutil.Int64ToBytes(seq)
		am[&pathpre] = goutil.Int64ToBytes(id)
		pathseqkey := append(PATH_SEQ, goutil.Int64ToBytes(id)...)
		am[&pathseqkey] = bean.Value
		wfsdb.Batch(am, dm)
		return
	}

	if len(bean.Key) > 2 && bytes.Equal(bean.Key[:2], PATH_PRE) && len(bean.Value) == 8 {
		paths := bean.Key[2:]
		b := true
		for _, v := range paths {
			if v > unicode.MaxASCII {
				b = false
			}
		}
		if b && goutil.BytesToInt64(bean.Value) < 1<<50 {
			return
		}
	}
	if len(bean.Key) == 8 {
		catchDel(bean.Key)
	}
	err = wfsdb.LoadSnapshotBean(bean)
	return
}
