package topom

import (
	"math"

	"github.com/wandoulabs/codis/pkg/models"
	"github.com/wandoulabs/codis/pkg/proxy"
	"github.com/wandoulabs/codis/pkg/utils/atomic2"
	"github.com/wandoulabs/codis/pkg/utils/errors"
	"github.com/wandoulabs/codis/pkg/utils/log"
)

var (
	ErrInvalidGroupId = errors.New("invalid group id")

	ErrGroupExists         = errors.New("group already exists")
	ErrGroupNotExists      = errors.New("group does not exist")
	ErrGroupIsPromoting    = errors.New("group is promoting")
	ErrGroupIsNotPromoting = errors.New("group is not promoting")
	ErrGroupResyncSlots    = errors.New("group resync slots failed")
	ErrGroupIsEmpty        = errors.New("group is empty")
	ErrGroupIsNotEmpty     = errors.New("group is not empty")
	ErrGroupIsLocked       = errors.New("group is locked")

	ErrServerExists       = errors.New("server already exists")
	ErrServerNotExists    = errors.New("server does not exist")
	ErrServerIsBusy       = errors.New("server is still busy")
	ErrServerPromoteAgain = errors.New("server is already master")
)

func (s *Topom) GetGroupModels() []*models.Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getGroupModels()
}

func (s *Topom) getGroupModels() []*models.Group {
	glist := make([]*models.Group, 0, len(s.groups))
	for _, g := range s.groups {
		glist = append(glist, g)
	}
	models.SortGroup(glist, func(g1, g2 *models.Group) bool {
		return g1.Id < g2.Id
	})
	return glist
}

func (s *Topom) getGroup(groupId int) (*models.Group, error) {
	if g := s.groups[groupId]; g != nil {
		return g, nil
	}
	return nil, errors.Trace(ErrGroupNotExists)
}

func (s *Topom) getGroupMaster(groupId int) string {
	if g := s.groups[groupId]; g != nil && len(g.Servers) != 0 {
		return g.Servers[0]
	}
	return ""
}

func (s *Topom) isGroupPromoting(groupId int) bool {
	if g := s.groups[groupId]; g != nil {
		return g.Promoting
	}
	return false
}

func (s *Topom) acquireGroupLock(groupId int) {
	if l := s.glocks[groupId]; l != nil {
		if n := l.Incr(); n > 128 {
			log.Warnf("[%p] glocks-[%d] increase to %d", s, groupId, n)
		}
	}
}

func (s *Topom) releaseGroupLock(groupId int) {
	if l := s.glocks[groupId]; l != nil {
		if n := l.Decr(); n < 0 {
			log.Panicf("[%p] glocks-[%d] decrease to %d", s, groupId, n)
		}
	}
}

func (s *Topom) isGroupLocked(groupId int) bool {
	if l := s.glocks[groupId]; l != nil {
		return l.Get() != 0
	}
	return false
}

func (s *Topom) CreateGroup(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if groupId <= 0 || groupId > math.MaxInt16 {
		return errors.Trace(ErrInvalidGroupId)
	}
	if s.groups[groupId] != nil {
		return errors.Trace(ErrGroupExists)
	}

	g := &models.Group{
		Id:      groupId,
		Servers: []string{},
	}
	if err := s.store.CreateGroup(groupId, g); err != nil {
		log.ErrorErrorf(err, "[%p] create group-[%d] failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	s.groups[groupId] = g
	s.glocks[groupId] = &atomic2.Int64{}

	log.Infof("[%p] create group-[%d]:\n%s", s, groupId, g.Encode())

	return nil
}

func (s *Topom) RemoveGroup(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if len(g.Servers) != 0 {
		return errors.Trace(ErrGroupIsNotEmpty)
	}

	if err := s.store.RemoveGroup(groupId); err != nil {
		log.ErrorErrorf(err, "[%p] remove group-[%d] failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	delete(s.groups, groupId)
	delete(s.glocks, groupId)

	log.Infof("[%p] remove group-[%d]:\n%s", s, groupId, g.Encode())

	return nil
}

func (s *Topom) GroupAddServer(groupId int, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if _, ok := s.stats.servers[addr]; ok {
		return errors.Trace(ErrServerExists)
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.Trace(ErrGroupIsPromoting)
	}

	n := &models.Group{
		Id:      groupId,
		Servers: append(g.Servers, addr),
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.ErrorErrorf(err, "[%p] group-[%d] update failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	s.groups[groupId] = n
	s.stats.servers[addr] = nil

	log.Infof("[%p] update group-[%d]:\n%s", s, groupId, n.Encode())

	return nil
}

func (s *Topom) GroupDelServer(groupId int, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if _, ok := s.stats.servers[addr]; !ok {
		return errors.Trace(ErrServerNotExists)
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.Trace(ErrGroupIsPromoting)
	}

	servers := []string{}
	for _, x := range g.Servers {
		if x != addr {
			servers = append(servers, x)
		}
	}
	if len(g.Servers) == len(servers) {
		return errors.Trace(ErrServerNotExists)
	}
	if addr == g.Servers[0] {
		if len(g.Servers) != 1 || len(s.getSlotsByGroup(groupId)) != 0 {
			return errors.Trace(ErrServerIsBusy)
		}
	}

	n := &models.Group{
		Id:      groupId,
		Servers: servers,
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.ErrorErrorf(err, "[%p] group-[%d] update failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	s.groups[groupId] = n
	delete(s.stats.servers, addr)

	log.Infof("[%p] update group-[%d]:\n%s", s, groupId, n.Encode())

	return nil
}

func (s *Topom) GroupPromoteServer(groupId int, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if s.isGroupLocked(groupId) {
		return errors.Trace(ErrGroupIsLocked)
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if g.Promoting {
		return errors.Trace(ErrGroupIsPromoting)
	}

	servers := []string{}
	for _, x := range g.Servers {
		if x != addr {
			servers = append(servers, x)
		}
	}
	if len(g.Servers) == len(servers) {
		return errors.Trace(ErrServerNotExists)
	}
	if addr == g.Servers[0] {
		return errors.Trace(ErrServerPromoteAgain)
	}

	n := &models.Group{
		Id:        groupId,
		Servers:   append([]string{addr}, servers...),
		Promoting: true,
	}
	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.ErrorErrorf(err, "[%p] group-[%d] update failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	s.groups[groupId] = n

	log.Infof("[%p] update group-[%d]:\n%s", s, groupId, n.Encode())

	return nil
}

func (s *Topom) GroupPromoteCommit(groupId int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedTopom
	}

	if s.isGroupLocked(groupId) {
		return errors.Trace(ErrGroupIsLocked)
	}

	g, err := s.getGroup(groupId)
	if err != nil {
		return err
	}
	if !g.Promoting {
		return errors.Trace(ErrGroupIsNotPromoting)
	}

	if err := s.resyncGroup(groupId); err != nil {
		return err
	}

	n := &models.Group{
		Id:        groupId,
		Servers:   g.Servers,
		Promoting: false,
	}
	s.groups[groupId] = n

	var rollback = true
	defer func() {
		if rollback {
			s.groups[groupId] = g
		}
	}()

	if err := s.resyncGroup(groupId); err != nil {
		return err
	}

	if err := s.store.UpdateGroup(groupId, n); err != nil {
		log.ErrorErrorf(err, "[%p] group-[%d] update failed", s, groupId)
		return errors.Trace(ErrUpdateStore)
	}

	rollback = false

	log.Infof("[%p] update group-[%d]:\n%s", s, groupId, n.Encode())

	return nil
}

func (s *Topom) resyncGroup(groupId int) error {
	slots := s.getSlotsByGroup(groupId)
	if len(slots) == 0 {
		return nil
	}
	errs := s.broadcast(func(p *models.Proxy, c *proxy.ApiClient) error {
		if err := c.FillSlots(slots...); err != nil {
			log.WarnErrorf(err, "[%p] proxy-[%s] resync group-[%d] failed", s, p.Token, groupId)
			return errors.Trace(ErrProxyRpcFailed)
		}
		return nil
	})
	for _, err := range errs {
		if err != nil {
			return errors.Trace(ErrGroupResyncSlots)
		}
	}
	return nil
}
