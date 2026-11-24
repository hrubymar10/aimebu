package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (service *Service) CreateUserInvite(ctx context.Context, actorID, kind, projectID, role, targetID string) (store.UserInvite, string, error) {
	token, err := randomToken("aui_")
	if err != nil {
		return store.UserInvite{}, "", err
	}
	invite, err := service.Store.CreateUserInvite(ctx, actorID, kind, projectID, role, targetID, hashToken(token), service.Now().UTC().Add(time.Hour))
	return invite, token, err
}

func validLogin(login string) bool {
	if len(login) < 3 || len(login) > 32 || login[0] < 'a' || login[0] > 'z' {
		return false
	}
	for _, ch := range login[1:] {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func inviteThrottleKey(token string) string { return "user-invite:" + hashToken(token) }

// InspectUserInvite tells the holder of a link what it is for, under the same throttle as using it: a wrong token counts as a failure.
func (service *Service) InspectUserInvite(ctx context.Context, token, remoteIP string) (store.InviteInfo, error) {
	now := service.Now().UTC()
	throttleKey := inviteThrottleKey(token)
	if !service.PairThrottle.reserve(throttleKey, remoteIP, now) {
		return store.InviteInfo{}, ErrRateLimited
	}
	defer service.PairThrottle.release(throttleKey, remoteIP)
	if !strings.HasPrefix(token, "aui_") || len(token) < 40 {
		service.PairThrottle.fail(throttleKey, remoteIP, now)
		return store.InviteInfo{}, store.ErrInviteInvalid
	}
	info, err := service.Store.InspectUserInvite(ctx, hashToken(token), now)
	if err == store.ErrInviteInvalid {
		service.PairThrottle.fail(throttleKey, remoteIP, service.Now().UTC())
	}
	return info, err
}

func (service *Service) InviteRetryAfter(token, remoteIP string) time.Duration {
	return service.PairThrottle.RetryAfter(inviteThrottleKey(token), remoteIP, service.Now())
}

func (service *Service) RedeemUserInvite(ctx context.Context, token, login, password, existingID, remoteIP string) (store.UserInvite, string, string, error) {
	now := service.Now().UTC()
	throttleKey := inviteThrottleKey(token)
	if !service.PairThrottle.reserve(throttleKey, remoteIP, now) {
		return store.UserInvite{}, "", "", ErrRateLimited
	}
	defer service.PairThrottle.release(throttleKey, remoteIP)
	if !strings.HasPrefix(token, "aui_") || len(token) < 40 {
		service.PairThrottle.fail(throttleKey, remoteIP, service.Now().UTC())
		return store.UserInvite{}, "", "", store.ErrInviteInvalid
	}
	login = strings.ToLower(strings.TrimSpace(login))
	var hash, params string
	if existingID == "" && password == "" {
		return store.UserInvite{}, "", "", store.ErrInvalidTransition
	}
	if login != "" && !validLogin(login) || password != "" && len(password) < 12 {
		return store.UserInvite{}, "", "", store.ErrInvalidTransition
	}
	if password != "" {
		var err error
		hash, params, err = HashPassword(password, service.Policy)
		if err != nil {
			return store.UserInvite{}, "", "", err
		}
	}
	invite, userID, err := service.Store.RedeemUserInvite(ctx, hashToken(token), login, hash, params, existingID, now)
	if err != nil {
		// A login that is taken counts like a wrong token: the invite is not used up, so without it a login could be guessed without limit.
		if err == store.ErrInviteInvalid || errors.Is(err, store.ErrLoginTaken) {
			service.PairThrottle.fail(throttleKey, remoteIP, service.Now().UTC())
		}
		return store.UserInvite{}, "", "", err
	}
	service.PairThrottle.reset(throttleKey, remoteIP)
	user, err := service.Store.UserByID(ctx, userID)
	if err != nil {
		return store.UserInvite{}, "", "", err
	}
	return invite, userID, user.Login, nil
}
