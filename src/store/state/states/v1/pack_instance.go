package v1

import (
	"github.com/skycoin/bbs/src/misc/boo"
	"github.com/skycoin/bbs/src/store/io"
	"github.com/skycoin/bbs/src/store/object"
	"github.com/skycoin/cxo/skyobject"
	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/cipher/encoder"
	"sync"
)

const (
	nameThread = "thread"
	namePost   = "post"
)

type PackInstance struct {
	prev *PackInstance // Only temporary (for generating changes only).

	packMux sync.Mutex
	pack    *skyobject.Pack
	changes *io.Changes

	gotStore    *GotStore
	tVotesStore *ContentVotesStore
	pVotesStore *ContentVotesStore
	uVotesStore *UserVotesStore
	followStore *FollowPageStore
}

func NewPackInstance(oldInstance *PackInstance, pack *skyobject.Pack) (*PackInstance, error) {
	newInstance := &PackInstance{
		prev: oldInstance,
		pack: pack,
		changes: io.NewChanges(
			pack.Root().Pub,
			oldInstance != nil, // Only record changes if we have old pack instance.
		),
		followStore: NewFollowPageStore(),
	}
	if e := newInstance.extract(); e != nil {
		return nil, e
	}
	newInstance.prev = nil
	return newInstance, nil
}

func (p *PackInstance) extract() error {

	children, e := extractRootChildren(p.pack)
	if e != nil {
		return e
	}

	// Get old stores from previous PackInstance (if any).

	var oldGS *GotStore
	var oldTVS *ContentVotesStore
	var oldPVS *ContentVotesStore
	var oldUPS *UserVotesStore

	if p.prev != nil {
		oldGS = p.prev.gotStore
		oldTVS = p.prev.tVotesStore
		oldPVS = p.prev.pVotesStore
		oldUPS = p.prev.uVotesStore
	}

	// Initiate GotStore.
	tPages, has := children[indexContent].(*object.ThreadPages)
	if !has {
		return boo.New(boo.InvalidRead,
			"root child 'ThreadPages' is invalid")
	}
	p.gotStore, e = NewGotStore(
		oldGS,
		getRootChildHash(p.pack, indexContent),
		tPages,
		p.changes,
	)
	if e != nil {
		return e
	}

	// Process Deleted.
	deleted, has := children[indexDeleted].(*object.Deleted)
	if !has {
		return boo.New(boo.InvalidRead,
			"root child 'Deleted' is invalid")
	}
	for _, ref := range deleted.Threads {
		p.changes.RecordDeleteThread(ref)
	}
	for _, ref := range deleted.Posts {
		var tRef cipher.SHA256
		if oldGS != nil {
			tRef = oldGS.GetPostOrigin(ref)
		}
		p.changes.RecordDeletePost(tRef, ref)
	}

	// Initiate ThreadVotesStore.
	tvPages, has := children[indexThreadVotes].(*object.ThreadVotesPages)
	if !has {
		return boo.New(boo.InvalidRead,
			"root child 'ThreadVotesPages' is invalid")
	}
	p.tVotesStore, e = NewContentVotesStore(
		oldTVS,
		nameThread,
		getRootChildHash(p.pack, indexThreadVotes),
		tvPages.Threads,
		p.changes,
	)
	if e != nil {
		return e
	}

	// Initiate PostVotesStore.
	pvPages, has := children[indexPostVotes].(*object.PostVotesPages)
	if !has {
		return boo.New(boo.InvalidRead,
			"root child 'PostVotesPages' is invalid")
	}
	p.tVotesStore, e = NewContentVotesStore(
		oldPVS,
		namePost,
		getRootChildHash(p.pack, indexPostVotes),
		pvPages.Posts,
		p.changes,
	)
	if e != nil {
		return e
	}

	// Initiate UserVotesStore.
	uvPages, has := children[indexUserVotes].(*object.UserVotesPages)
	if !has {
		return boo.New(boo.InvalidRead,
			"root child 'UserVotesPages' is invalid")
	}
	p.uVotesStore, e = NewUserVotesStore(
		oldUPS,
		getRootChildHash(p.pack, indexUserVotes),
		uvPages.Users,
		// Up-vote action.
		func(v *object.Vote) {
			p.followStore.Modify(v.Creator).Yes[v.OfUser.Hex()] =
				object.Tag{Mode: "+1", Text: string(v.Tag)}
		},
		// Down-vote action.
		func(v *object.Vote) {
			p.followStore.Modify(v.Creator).No[v.OfUser.Hex()] =
				object.Tag{Mode: "-1", Text: string(v.Tag)}
		},
		p.changes,
	)
	if e != nil {
		return e
	}

	return nil
}

func (p *PackInstance) Do(action func(pi *PackInstance) error) error {
	p.packMux.Lock()
	defer p.packMux.Unlock()
	return action(p)
}

func (p *PackInstance) GetThreadPages() (*object.ThreadPages, error) {
	tPagesVal, e := p.pack.RefByIndex(indexContent)
	if e != nil {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"failed to obtain root child value of index", indexContent)
	}
	tPages, ok := tPagesVal.(*object.ThreadPages)
	if !ok {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"root child 'ThreadPages' is invalid")
	}
	return tPages, nil
}

func (p *PackInstance) GetThreadVotesPages() (*object.ThreadVotesPages, error) {
	tvPagesVal, e := p.pack.RefByIndex(indexThreadVotes)
	if e != nil {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"failed to obtain root child value of index", indexThreadVotes)
	}
	tvPages, ok := tvPagesVal.(*object.ThreadVotesPages)
	if !ok {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"root child 'ThreadVotesPages' is invalid")
	}
	return tvPages, nil
}

func (p *PackInstance) GetPostVotesPages() (*object.PostVotesPages, error) {
	pvPagesVal, e := p.pack.RefByIndex(indexPostVotes)
	if e != nil {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"failed to obtain root child of index", indexPostVotes)
	}
	pvPages, ok := pvPagesVal.(*object.PostVotesPages)
	if !ok {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"root child 'PostVotesPages' is invalid")
	}
	return pvPages, nil
}

func (p *PackInstance) AppendThreadVotesPage(tHash cipher.SHA256) error {
	// Check it's existence in compiled store.
	// Return if already exists.
	if _, e := p.tVotesStore.Get(tHash); e == nil {
		return nil
	}

	// Append ThreadVotesPage to ThreadVotesPages.
	tvPages, e := p.GetThreadVotesPages()
	if e != nil {
		return e
	}
	vPage := object.ContentVotesPage{Ref: tHash}
	tvPages.Threads = append(tvPages.Threads, vPage)

	// Save to pack.
	e = p.pack.SetRefByIndex(indexThreadVotes, tvPages)
	if e != nil {
		return e
	}

	// Save to compiled store.
	p.tVotesStore.Set(tHash, &object.VotesSummary{
		Index:     len(tvPages.Threads) - 1,
		OfContent: tHash,
		Hash:      cipher.SumSHA256(encoder.Serialize(vPage)),
		Votes:     make(map[cipher.PubKey]object.Vote),
	})

	return nil
}

func (p *PackInstance) AppendPostVotesPage(pHash cipher.SHA256) error {
	// Check it's existence is compiled store.
	// Return if already exists.
	if _, e := p.pVotesStore.Get(pHash); e == nil {
		return nil
	}

	// Append PostVotesPage to PostVotesPages.
	pvPages, e := p.GetPostVotesPages()
	if e != nil {
		return e
	}
	vPage := object.ContentVotesPage{Ref: pHash}
	pvPages.Posts = append(pvPages.Posts, vPage)

	// Save to pack.
	e = p.pack.SetRefByIndex(indexPostVotes, pvPages)
	if e != nil {
		return e
	}

	// Save to compiled store.
	p.pVotesStore.Set(pHash, &object.VotesSummary{
		Index:     len(pvPages.Posts) - 1,
		OfContent: pHash,
		Hash:      cipher.SumSHA256(encoder.Serialize(vPage)),
		Votes:     make(map[cipher.PubKey]object.Vote),
	})

	return nil
}

/*
	<<< HELPER FUNCTIONS >>>
*/

func extractRootChildren(pack *skyobject.Pack) ([]interface{}, error) {
	rc, e := pack.RootRefs()
	if e != nil {
		return nil, boo.WrapType(e, boo.InvalidRead,
			"failed to extract root children")
	}
	if len(rc) != countRootRefs {
		return nil, boo.Newf(boo.InvalidRead,
			"root has invalid ref count of %d when expecting %d",
			len(rc), countRootRefs)
	}
	return rc, nil
}

func getRootChildHash(pack *skyobject.Pack, i int) cipher.SHA256 {
	return pack.Root().Refs[i].Object
}
