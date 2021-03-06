package state

import (
	"github.com/skycoin/bbs/src/misc/boo"
	"github.com/skycoin/bbs/src/misc/inform"
	"github.com/skycoin/bbs/src/misc/typ"
	"github.com/skycoin/bbs/src/misc/typ/paginatedtypes"
	"github.com/skycoin/bbs/src/store/object"
	"github.com/skycoin/cxo/skyobject"
	"github.com/skycoin/skycoin/src/cipher"
	"log"
	"math"
	"os"
	"sync"
)

// ErrViewerNotInitialized occurs when the Viewer is not initiated.
var ErrViewerNotInitialized = boo.New(boo.NotFound, "viewer is not initialized")

/*
	<<< INDEXER >>>
*/

// Indexer is responsible for indexing and holding hashes for content.
type Indexer struct {
	Board         string
	Threads       typ.Paginated
	PostsOfThread map[string]typ.Paginated // key (hash of thread or post), value (list of posts)
	Users         typ.Paginated
}

// NewIndexer creates a new Indexer.
func NewIndexer() *Indexer {
	return &Indexer{
		Threads:       paginatedtypes.NewSimple(),
		PostsOfThread: make(map[string]typ.Paginated),
		Users:         paginatedtypes.NewMapped(),
	}
}

// EnsureUsersOfUserVoteBody ensures that user participants of a given vote body,
// has associated user profile indexes saved in the Indexer.
func (i *Indexer) EnsureUsersOfUserVoteBody(body *object.Body) {
	i.Users.Append(body.Creator)
	i.Users.Append(body.OfUser)
}

/*
	<<< CONTAINER >>>
*/

// Container contains the objects the the Indexer indexes.
type Container struct {
	content  map[string]*object.ContentRep
	votes    map[string]*VotesRep
	profiles map[string]*Profile
}

// NewContainer creates a new Container.
func NewContainer() *Container {
	return &Container{
		content:  make(map[string]*object.ContentRep),
		votes:    make(map[string]*VotesRep),
		profiles: make(map[string]*Profile),
	}
}

// GetProfile obtains a profile object from the container.
// If the profile does not exist, it is created and the newly created profile is returned.
func (c *Container) GetProfile(upk string) *Profile {
	if profile, ok := c.profiles[upk]; ok {
		return profile
	} else {
		profile = NewProfile()
		c.profiles[upk] = profile
		return profile
	}
}

/*
	<<< VIEWER >>>
*/

// Viewer generates and compiles views for the board.
type Viewer struct {
	mux sync.Mutex
	l   *log.Logger
	pk  cipher.PubKey
	i   *Indexer
	c   *Container
}

// NewViewer creates a new viewer with a given pack.
func NewViewer(pack *skyobject.Pack) (*Viewer, error) {
	v := &Viewer{
		l:  inform.NewLogger(true, os.Stdout, "STATE_VIEWER"),
		pk: pack.Root().Pub,
		i:  NewIndexer(),
		c:  NewContainer(),
	}

	pages, e := object.GetPages(pack, &object.GetPagesIn{
		RootPage:  false,
		BoardPage: true,
		DiffPage:  false,
		UsersPage: true,
	})
	if e != nil {
		return nil, e
	}

	// Set board.
	if board, e := pages.BoardPage.GetBoard(); e != nil {
		return nil, e
	} else {
		v.setBoard(board)
	}

	e = pages.BoardPage.RangeThreadPages(func(i int, tp *object.ThreadPage) error {
		thread, e := tp.GetThread()
		if e != nil {
			return e
		}
		tBody, tHeader := thread.GetBody(), thread.GetHeader()
		v.ensureUser(tBody.Creator)
		tHash, e := v.addThread(thread, tBody, tHeader)
		if e != nil {
			return e
		}
		return tp.RangePosts(func(i int, post *object.Content) error {
			pBody, pHeader := post.GetBody(), post.GetHeader()
			v.ensureUser(pBody.Creator)
			return v.addPost(tHash, post, pBody, pHeader)
		})
	})
	if e != nil {
		return nil, e
	}

	e = pages.UsersPage.RangeUserProfiles(func(i int, uap *object.UserProfile) error {
		return uap.RangeSubmissions(func(i int, c *object.Content) error {
			vBody, vHeader := c.GetBody(), c.GetHeader()
			v.ensureUser(vBody.Creator)
			if e := v.processVote(c, vBody, vHeader); e != nil {
				return e
			}
			return nil
		})
	})
	if e != nil {
		return nil, e
	}

	return v, nil
}

// Update updates the viewer with new pack and headers.
func (v *Viewer) Update(pack *skyobject.Pack, headers *Headers) error {
	if v == nil {
		return ErrViewerNotInitialized
	}
	defer v.lock()()

	pages, e := object.GetPages(pack, &object.GetPagesIn{
		RootPage:  false,
		BoardPage: true,
		DiffPage:  false,
		UsersPage: false,
	})
	if e != nil {
		return e
	}

	board, e := pages.BoardPage.GetBoard()
	if e != nil {
		return e
	}
	v.setBoard(board)

	for _, content := range headers.GetChanges().New {
		var (
			header = content.GetHeader()
			body   = content.GetBody()
		)

		v.ensureUser(body.Creator)

		switch body.Type {
		case object.V5ThreadType:
			if _, e := v.addThread(content, body, header); e != nil {
				return e
			}
		case object.V5PostType:
			tHash, _ := body.GetOfThread()
			if e := v.addPost(tHash, content, body, header); e != nil {
				return e
			}
		case object.V5ThreadVoteType, object.V5PostVoteType, object.V5UserVoteType:
			v.processVote(content, body, header)
		}
	}

	return nil
}

func (v *Viewer) lock() func() {
	v.mux.Lock()
	return v.mux.Unlock
}

func (v *Viewer) setBoard(bc *object.Content) {
	delete(v.c.content, v.i.Board)
	v.i.Board = bc.GetHeader().Hash
	rep := bc.ToRep()
	rep.PubKey = v.pk.Hex()
	v.c.content[v.i.Board] = rep
}

func (v *Viewer) addThread(tc *object.Content, b *object.Body, h *object.ContentHeaderData) (cipher.SHA256, error) {

	// Check board public key.
	if e := checkBoardRef(v.pk, b, "thread"); e != nil {
		return cipher.SHA256{}, e
	}

	tHash := h.GetHash()
	v.i.Threads.Append(tHash.Hex())
	v.c.content[tHash.Hex()] = tc.ToRep()
	v.i.PostsOfThread[tHash.Hex()] = paginatedtypes.NewMapped()
	return tHash, nil
}

func (v *Viewer) addPost(tHash cipher.SHA256, pc *object.Content, b *object.Body, h *object.ContentHeaderData) error {

	// Check board public key.
	if e := checkBoardRef(v.pk, b, "post"); e != nil {
		return e
	}

	// Check thread ref.
	if e := checkThreadRef(tHash, b, "post"); e != nil {
		return e
	}

	pHash := h.Hash
	if posts, ok := v.i.PostsOfThread[tHash.Hex()]; !ok {
		return boo.Newf(boo.Internal, "thread of hash %s not found", tHash.Hex())
	} else {
		posts.Append(pHash)
		v.c.content[pHash] = pc.ToRep()
	}

	if ofPost, _ := b.GetOfPost(); ofPost != (cipher.SHA256{}) {
		pList, ok := v.i.PostsOfThread[ofPost.Hex()]
		if !ok {
			pList = paginatedtypes.NewMapped()
			v.i.PostsOfThread[ofPost.Hex()] = pList
		}
		pList.Append(pHash)
	}

	return nil
}

func (v *Viewer) ensureUser(upk string) {
	v.i.Users.Append(upk)
	if _, ok := v.c.profiles[upk]; !ok {
		v.c.profiles[upk] = NewProfile()
	}
}

func (v *Viewer) processVote(c *object.Content, b *object.Body, h *object.ContentHeaderData) error {
	var cHash string
	var cType object.ContentType

	// Only if vote is for post or thread.
	switch b.Type {
	case object.V5ThreadVoteType:
		cHash = b.OfThread
		cType = object.V5ThreadVoteType

	case object.V5PostVoteType:
		cHash = b.OfPost
		cType = object.V5PostVoteType

	case object.V5UserVoteType:
		return v.processUserVote(c, b, h)

	default:
		return nil
	}

	if v.c.content[cHash] == nil {
		return nil
	}

	// Add to votes map.
	voteRep, has := v.c.votes[cHash]
	if !has {
		voteRep = new(VotesRep).Fill(cType, cHash)
		v.c.votes[cHash] = voteRep
	}
	voteRep.Add(c)

	return nil
}

func (v *Viewer) processUserVote(c *object.Content, b *object.Body, h *object.ContentHeaderData) error {
	var (
		creatorProfile = v.c.GetProfile(b.Creator)
		ofUserProfile  = v.c.GetProfile(b.OfUser)
	)

	creatorProfile.ClearVotesFor(b.OfUser)
	ofUserProfile.ClearVotesBy(b.Creator)

	switch b.Value {
	case +1:
		if b.HasTag(object.TrustTag) {
			v.i.EnsureUsersOfUserVoteBody(b)
			creatorProfile.Trusted[b.OfUser] = struct{}{}
			ofUserProfile.TrustedBy[b.Creator] = struct{}{}
		}
	case -1:
		if b.HasTag(object.SpamTag) {
			v.i.EnsureUsersOfUserVoteBody(b)
			creatorProfile.MarkedAsSpam[b.OfUser] = struct{}{}
			ofUserProfile.MarkedAsSpamBy[b.Creator] = struct{}{}
		}
		if b.HasTag(object.BlockTag) {
			v.i.EnsureUsersOfUserVoteBody(b)
			creatorProfile.Blocked[b.OfUser] = struct{}{}
			ofUserProfile.BlockedBy[b.Creator] = struct{}{}
		}
	case 0:
		v.i.EnsureUsersOfUserVoteBody(b)
	}
	return nil
}

/*
	<<< CHECK >>>
*/

func (v *Viewer) HasUser(upk string) bool {
	if v == nil {
		return false
	}
	defer v.lock()()
	return v.i.Users.Has(upk)
}

func (v *Viewer) HasThread(tHash string) bool {
	if v == nil {
		return false
	}
	defer v.lock()()
	return v.i.Threads.Has(tHash)
}

func (v *Viewer) HasContent(hash string) bool {
	if v == nil {
		return false
	}
	defer v.lock()()
	_, ok := v.c.content[hash]
	return ok
}

/*
	<<< GET >>>
*/

// GetBoard gets a single board's data.
func (v *Viewer) GetBoard() (*object.ContentRep, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()
	return v.c.content[v.i.Board], nil
}

// BoardPageIn represents the input required to obtain board page.
type BoardPageIn struct {
	Perspective    string
	PaginatedInput typ.PaginatedInput
}

// BoardPageOut represents the output for board page.
type BoardPageOut struct {
	Board *object.ContentRep `json:"board"`
	//ThreadsMeta *typ.PaginatedOutput `json:"threads_meta"`
	Threads []*object.ContentRep `json:"threads"`
}

// GetBoardPage obtains a board page.
func (v *Viewer) GetBoardPage(in *BoardPageIn) (*BoardPageOut, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()

	tHashes, e := v.i.Threads.Get(&in.PaginatedInput)
	if e != nil {
		return nil, e
	}

	out := new(BoardPageOut)
	out.Board = v.c.content[v.i.Board]
	//out.ThreadsMeta = tHashes
	out.Threads = make([]*object.ContentRep, len(tHashes.Data))
	for i, tHash := range tHashes.Data {
		out.Threads[i] = v.c.content[tHash]
		if votes, ok := v.c.votes[tHash]; ok {
			out.Threads[i].Votes = votes.View(in.Perspective)
		}
	}
	return out, nil
}

// ThreadPageIn represents the input required to obtain thread page.
type ThreadPageIn struct {
	Perspective    string
	ThreadHash     string
	PaginatedInput typ.PaginatedInput
}

// ThreadPageOut represents the output for thread page.
type ThreadPageOut struct {
	Board  *object.ContentRep `json:"board"`
	Thread *object.ContentRep `json:"thread"`
	//PostsMeta *typ.PaginatedOutput `json:"posts_meta"`
	Posts []*object.ContentRep `json:"posts"`
}

// GetThreadPage obtains the thread page.
func (v *Viewer) GetThreadPage(in *ThreadPageIn) (*ThreadPageOut, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()
	out := new(ThreadPageOut)
	out.Board = v.c.content[v.i.Board]
	out.Thread = v.c.content[in.ThreadHash]

	if out.Thread == nil {
		return nil, boo.Newf(boo.NotFound, "thread of hash '%s' is not found in board '%s'",
			in.ThreadHash, v.pk.Hex())
	}
	if votes, ok := v.c.votes[in.ThreadHash]; ok {
		out.Thread.Votes = votes.View(in.Perspective)
	}

	pHashes, e := v.i.PostsOfThread[in.ThreadHash].Get(&in.PaginatedInput)
	if e != nil {
		return nil, e
	}
	out.Posts = make([]*object.ContentRep, len(pHashes.Data))
	for i, pHash := range pHashes.Data {
		out.Posts[i] = v.c.content[pHash]
		if votes, ok := v.c.votes[pHash]; ok {
			out.Posts[i].Votes = votes.View(in.Perspective)
		}
	}

	return out, nil
}

// ContentVotesIn represents the input required to obtain content votes.
type ContentVotesIn struct {
	Perspective string
	ContentHash string
}

// ContentVotesOut represents the output for content votes.
type ContentVotesOut struct {
	Votes *VoteRepView `json:"votes"`
}

// GetVotes obtains content votes.
func (v *Viewer) GetVotes(in *ContentVotesIn) (*ContentVotesOut, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()
	out := new(ContentVotesOut)
	if votes, ok := v.c.votes[in.ContentHash]; ok {
		out.Votes = votes.View(in.Perspective)
		return out, nil
	}
	if _, ok := v.c.content[in.ContentHash]; ok {
		out.Votes = &VoteRepView{
			Ref: in.ContentHash,
		}
		return out, nil
	}
	return nil, boo.Newf(boo.NotFound, "content of hash '%s' is not found",
		in.ContentHash)
}

type UserProfileIn struct {
	UserPubKey string
}

type UserProfileOut struct {
	UserPubKey string       `json:"user_public_key"`
	Profile    *ProfileView `json:"profile"`
}

func (v *Viewer) GetUserProfile(in *UserProfileIn) (*UserProfileOut, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()
	if !v.i.Users.Has(in.UserPubKey) {
		return nil, boo.Newf(boo.NotFound,
			"user of public key %s is not found", in.UserPubKey)
	}
	profile, ok := v.c.profiles[in.UserPubKey]
	if !ok {
		return nil, boo.Newf(boo.Internal,
			"user of public key %s is indexed but has no profile", in.UserPubKey)
	}
	return &UserProfileOut{
		UserPubKey: in.UserPubKey,
		Profile:    profile.View(),
	}, nil
}

type ParticipantsOut struct {
	Participants []string `json:"participants"`
}

func (v *Viewer) GetParticipants() (*ParticipantsOut, error) {
	if v == nil {
		return nil, ErrViewerNotInitialized
	}
	defer v.lock()()
	out, e := v.i.Users.Get(&typ.PaginatedInput{
		StartIndex: 0,
		PageSize:   math.MaxUint64,
	})
	if e != nil {
		return nil, e
	}
	return &ParticipantsOut{
		Participants: out.Data,
	}, nil
}

/*
	<<< HELPER FUNCTIONS >>>
*/

func checkBoardRef(expected cipher.PubKey, body *object.Body, what string) error {
	if got, e := body.GetOfBoard(); e != nil {
		return boo.WrapTypef(e, boo.InvalidRead, "corrupt %s", what)
	} else if got != expected {
		return boo.Newf(boo.InvalidRead,
			"misplaced %s, unmatched board public key", what)
	} else {
		return nil
	}
}

func checkThreadRef(expected cipher.SHA256, body *object.Body, what string) error {
	if got, e := body.GetOfThread(); e != nil {
		return boo.WrapTypef(e, boo.InvalidRead, "corrupt %s", what)
	} else if got != expected {
		return boo.Newf(boo.InvalidRead,
			"misplaced %s, unmatched board public key", what)
	} else {
		return nil
	}
}
