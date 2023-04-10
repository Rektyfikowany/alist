package util

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"sync"
	"time"

	ipldformat "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-libipfs/files"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-mfs"
	pinningservice "github.com/ipfs/go-pinning-service-http-client"
	iface "github.com/ipfs/interface-go-ipfs-core"
	ifacepath "github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo"
	"github.com/ipfs/kubo/repo/fsrepo"
	"github.com/libp2p/go-libp2p/core/peer"
)

type MfsAPI struct {
	CID       *string
	PinID     *string
	lock      sync.RWMutex
	mroot     *mfs.Root
	pinclient *pinningservice.Client
}
type NodeObj struct {
	Id    string
	Name  string
	Size  int64
	Isdir bool
}

var Ctx = context.Background()
var DefaultPath = ""
var buildCount = -1
var buildLock sync.Mutex
var closeFunc func() error
var nodeApi iface.CoreAPI
var plugins = false
var repopath = ""

func NewMfs(purl, ptoken string) (mapi *MfsAPI, err error) {
	buildLock.Lock()
	defer buildLock.Unlock()
	if buildCount >= 0 {
		buildCount++
		return &MfsAPI{
			pinclient: pinningservice.NewClient(purl, ptoken),
		}, nil
	}
	buildCount = -1
	nodeApi = nil
	if !plugins {
		var pluginload *loader.PluginLoader
		if pluginload, err = loader.NewPluginLoader(""); err == nil {
			if err = pluginload.Initialize(); err == nil {
				if err = pluginload.Inject(); err == nil {
					plugins = true
				}
			}
		}
	}
	newcfg := &config.Config{}
	if err == nil {
		newcfg, err = config.Init(io.Discard, 2048)
	}
	if err == nil {
		repopath = path.Join(DefaultPath, "ipfs_"+newcfg.Identity.PeerID)
		defer func() {
			if mapi == nil {
				os.RemoveAll(repopath)
				repopath = ""
			}
		}()
		err = fsrepo.Init(repopath, newcfg)
	}
	var newrepo repo.Repo
	if err == nil {
		newrepo, err = fsrepo.Open(repopath)
	}
	newnode := &core.IpfsNode{}
	if err == nil {
		closeFunc = newrepo.Close
		defer func() {
			if mapi == nil {
				closeFunc()
				closeFunc = nil
			}
		}()
		newnode, err = core.NewNode(Ctx, &core.BuildCfg{Online: true, Repo: newrepo})
	}
	var newapi iface.CoreAPI
	if err == nil {
		closeFunc = newnode.Close
		newapi, err = coreapi.NewCoreAPI(newnode)
	}
	if err == nil {
		nodeApi = newapi
		buildCount = 1
		return &MfsAPI{
			pinclient: pinningservice.NewClient(purl, ptoken),
		}, nil
	}
	return
}
func (mapi *MfsAPI) Close() (err error) {
	mapi.lock.Lock()
	defer func() {
		buildLock.Lock()
		defer buildLock.Unlock()
		if buildCount--; buildCount <= 0 {
			buildCount = -1
			nodeApi = nil
			defer os.RemoveAll(repopath)
			repopath = ""
			closeFunc()
			closeFunc = nil
		}
	}()
	if mapi.mroot != nil {
		mapi.mroot.FlushMemFree(Ctx)
		mapi.mroot = nil
	}
	return
}
func (mapi *MfsAPI) newRoot(force bool) (err error) {
	if !force && mapi.mroot != nil {
		return
	}
	mapi.lock.Lock()
	defer mapi.lock.Unlock()
	if !force && mapi.mroot != nil {
		return
	}
	pinid := ""
	rootcid := ""
	if ptr := mapi.PinID; ptr != nil {
		pinid = *ptr
	}
	if ptr := mapi.CID; ptr != nil {
		rootcid = *ptr
	}
	if pinstatus, err := mapi.pinclient.GetStatusByID(Ctx, pinid); err == nil {
		if info, err := peer.AddrInfosFromP2pAddrs(pinstatus.GetDelegates()...); err == nil {
			for _, a := range info {
				go nodeApi.Swarm().Connect(Ctx, a)
			}
		}
		if pinstatus.GetStatus() == pinningservice.StatusPinned {
			rootcid = pinstatus.GetPin().GetCid().String()
		}
	}
	var ldnode ipldformat.Node
	if err == nil {
		tctx, tcancel := context.WithTimeout(Ctx, time.Minute)
		ldnode, err = nodeApi.ResolveNode(tctx, ifacepath.New(rootcid))
		tcancel()
	}
	prnode := &merkledag.ProtoNode{}
	if err == nil {
		ok := true
		if prnode, ok = ldnode.(*merkledag.ProtoNode); !ok {
			err = fmt.Errorf(reflect.TypeOf(ldnode).String())
		}
	}
	mroot := &mfs.Root{}
	if err == nil {
		mroot, err = mfs.NewRoot(Ctx, nodeApi.Dag(), prnode, nil)
	}
	if err == nil {
		ldnode, err = mroot.GetDirectory().GetNode()
	}
	if err == nil {
		if mapi.mroot != nil {
			mapi.mroot.FlushMemFree(Ctx)
		}
		mapi.mroot = mroot
		if ptr := mapi.CID; ptr != nil {
			*ptr = ldnode.Cid().String()
		}
	}
	return
}
func (mapi *MfsAPI) List(pth string) (ol []NodeObj, err error) {
	if err = mapi.newRoot(false); err != nil {
		return
	}
	mapi.lock.RLock()
	defer mapi.lock.RUnlock()
	snode, err := mfs.Lookup(mapi.mroot, pth)
	dnode, ok := snode.(*mfs.Directory)
	if err == nil && !ok {
		err = fmt.Errorf(reflect.TypeOf(snode).String())
	}
	nl := []mfs.NodeListing{}
	if err == nil {
		nl, err = dnode.List(Ctx)
	}
	if err == nil {
		ol = []NodeObj{}
		for _, n := range nl {
			ol = append(ol, NodeObj{
				Id:    n.Hash,
				Name:  n.Name,
				Size:  n.Size,
				Isdir: n.Type == int(mfs.TDir),
			})
		}
	}
	return ol, err
}
func (mapi *MfsAPI) Mkdir(pth string) (err error) {
	if err = mapi.newRoot(false); err != nil {
		return
	}
	mapi.lock.RLock()
	defer mapi.lock.RUnlock()
	if err = mfs.Mkdir(mapi.mroot, pth, mfs.MkdirOpts{}); err == nil {
		err = mapi.waitpin()
	}
	return
}
func (mapi *MfsAPI) Mv(src, dst string) (err error) {
	if err = mapi.newRoot(false); err != nil {
		return
	}
	mapi.lock.RLock()
	defer mapi.lock.RUnlock()
	if err = mfs.Mv(mapi.mroot, src, dst); err == nil {
		err = mapi.waitpin()
	}
	return
}
func (mapi *MfsAPI) Put(pth, nodecid string, rc io.ReadCloser) (err error) {
	if err = mapi.newRoot(false); err != nil {
		return
	}
	mapi.lock.RLock()
	defer mapi.lock.RUnlock()
	var rsnode ipldformat.Node
	if err == nil {
		rsnode, err = nodeApi.ResolveNode(Ctx, ifacepath.New(nodecid))
		if err != nil {
			var rspath ifacepath.Resolved
			if rspath, err = nodeApi.Unixfs().Add(Ctx, files.NewReaderFile(rc)); err == nil {
				rsnode, err = nodeApi.ResolveNode(Ctx, rspath)
			}
		}
	}
	if err == nil {
		err = mfs.PutNode(mapi.mroot, pth, rsnode)
	}
	if err == nil {
		err = mapi.waitpin()
	}
	return
}
func (mapi *MfsAPI) Unlink(pth, fname string) (err error) {
	if err = mapi.newRoot(false); err != nil {
		return
	}
	mapi.lock.RLock()
	defer mapi.lock.RUnlock()
	snode, err := mfs.Lookup(mapi.mroot, pth)
	dnode, ok := snode.(*mfs.Directory)
	if err == nil && !ok {
		err = fmt.Errorf(reflect.TypeOf(snode).String())
	}
	if err = dnode.Unlink(fname); err == nil {
		err = mapi.waitpin()
	}
	return
}
func (mapi *MfsAPI) waitpin() (err error) {
	err = mapi.mroot.Flush()
	if err == nil {
		err = fmt.Errorf("NotImplement")
	}
	return
}