package gsuite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/Redpatronus/AegiSheet/internal/config"
)

// Page size ceilings enforced by the Admin SDK Directory API.
const (
	maxUserPageSize   = 500
	maxGroupPageSize  = 200
	maxMemberPageSize = 200
)

// ErrStop ends an iteration early without reporting a failure. A visitor returns
// it when it has all the data it needs — for example once a configured row limit
// is reached — so the remaining pages are never fetched.
var ErrStop = errors.New("gsuite: iteration stopped by caller")

// Directory reads the Admin SDK Directory API.
type Directory struct {
	svc     *admin.Service
	api     config.APIConfig
	retrier *Retrier
	log     *slog.Logger

	// userCache memoises single-user lookups. The third-party token report resolves
	// the owner of every token, and a user commonly owns several.
	userMu    sync.Mutex
	userCache map[string]*admin.User
}

func newDirectory(svc *admin.Service, api config.APIConfig, retrier *Retrier, log *slog.Logger) *Directory {
	return &Directory{
		svc:       svc,
		api:       api,
		retrier:   retrier,
		log:       log,
		userCache: make(map[string]*admin.User),
	}
}

// scopeUsersCall applies the tenant selector to a users list call.
//
// The Admin SDK rejects a request that carries both customer and domain, so
// exactly one is set; domain wins when configured because it is the more specific
// instruction.
func (d *Directory) scopeUsersCall(call *admin.UsersListCall) *admin.UsersListCall {
	if d.api.Domain != "" {
		return call.Domain(d.api.Domain)
	}
	return call.Customer(d.api.Customer)
}

func (d *Directory) scopeGroupsCall(call *admin.GroupsListCall) *admin.GroupsListCall {
	if d.api.Domain != "" {
		return call.Domain(d.api.Domain)
	}
	return call.Customer(d.api.Customer)
}

// ForEachUser visits every user matching query, following the page chain.
//
// query uses the Directory API search syntax, e.g. "isEnrolledIn2Sv=false"; an
// empty query enumerates the whole tenant. Results are ordered by e-mail address
// so that two runs over unchanged data produce identical reports.
func (d *Directory) ForEachUser(ctx context.Context, query string, visit func(*admin.User) error) error {
	pageSize := clampPageSize(d.api.PageSize, maxUserPageSize)

	var (
		pageToken string
		pages     int
		visited   int
	)
	for {
		var page *admin.Users
		err := d.retrier.Do(ctx, "directory.users.list", func(ctx context.Context) error {
			call := d.svc.Users.List().
				MaxResults(pageSize).
				OrderBy("email").
				Context(ctx)
			call = d.scopeUsersCall(call)
			if query != "" {
				call = call.Query(query)
			}
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}

			var callErr error
			page, callErr = call.Do()
			return callErr
		})
		if err != nil {
			return fmt.Errorf("list users (query %q, page %d): %w", query, pages+1, err)
		}
		pages++

		for _, user := range page.Users {
			visited++
			if err := visit(user); err != nil {
				if errors.Is(err, ErrStop) {
					d.log.Debug("user iteration stopped early",
						slog.String("query", query), slog.Int("users", visited))
					return nil
				}
				return err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	d.log.Debug("users listed",
		slog.String("query", query), slog.Int("pages", pages), slog.Int("users", visited))
	return nil
}

// UserRef is the minimal identity of a user.
//
// Tasks that fan out over the whole tenant hold one of these per user instead of
// the full admin.User, which carries dozens of fields (aliases, addresses, custom
// schemas) that the per-user calls do not need. On a tenant with tens of
// thousands of accounts the difference is the between tens of megabytes and
// hundreds.
type UserRef struct {
	Email     string
	FullName  string
	Suspended bool
	Archived  bool
}

// ListUserRefs collects the identities of every user matching query.
func (d *Directory) ListUserRefs(ctx context.Context, query string) ([]UserRef, error) {
	var refs []UserRef
	err := d.ForEachUser(ctx, query, func(user *admin.User) error {
		refs = append(refs, UserRef{
			Email:     user.PrimaryEmail,
			FullName:  FullName(user),
			Suspended: user.Suspended,
			Archived:  user.Archived,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// GetUser resolves a single user, memoising the result.
func (d *Directory) GetUser(ctx context.Context, userKey string) (*admin.User, error) {
	d.userMu.Lock()
	if cached, ok := d.userCache[userKey]; ok {
		d.userMu.Unlock()
		return cached, nil
	}
	d.userMu.Unlock()

	var user *admin.User
	err := d.retrier.Do(ctx, "directory.users.get", func(ctx context.Context) error {
		var callErr error
		user, callErr = d.svc.Users.Get(userKey).Context(ctx).Do()
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	d.userMu.Lock()
	d.userCache[userKey] = user
	d.userMu.Unlock()

	return user, nil
}

// ForEachGroup visits every group in scope, following the page chain.
func (d *Directory) ForEachGroup(ctx context.Context, visit func(*admin.Group) error) error {
	pageSize := clampPageSize(d.api.PageSize, maxGroupPageSize)

	var (
		pageToken string
		pages     int
		visited   int
	)
	for {
		var page *admin.Groups
		err := d.retrier.Do(ctx, "directory.groups.list", func(ctx context.Context) error {
			call := d.svc.Groups.List().MaxResults(pageSize).Context(ctx)
			call = d.scopeGroupsCall(call)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}

			var callErr error
			page, callErr = call.Do()
			return callErr
		})
		if err != nil {
			return fmt.Errorf("list groups (page %d): %w", pages+1, err)
		}
		pages++

		for _, group := range page.Groups {
			visited++
			if err := visit(group); err != nil {
				if errors.Is(err, ErrStop) {
					return nil
				}
				return err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	d.log.Debug("groups listed", slog.Int("pages", pages), slog.Int("groups", visited))
	return nil
}

// ForEachMember visits the direct members of one group, following the page chain.
func (d *Directory) ForEachMember(ctx context.Context, groupKey string, visit func(*admin.Member) error) error {
	pageSize := clampPageSize(d.api.PageSize, maxMemberPageSize)

	var (
		pageToken string
		pages     int
	)
	for {
		var page *admin.Members
		err := d.retrier.Do(ctx, "directory.members.list", func(ctx context.Context) error {
			call := d.svc.Members.List(groupKey).MaxResults(pageSize).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}

			var callErr error
			page, callErr = call.Do()
			return callErr
		})
		if err != nil {
			return fmt.Errorf("list members of group (page %d): %w", pages+1, err)
		}
		pages++

		for _, member := range page.Members {
			if err := visit(member); err != nil {
				if errors.Is(err, ErrStop) {
					return nil
				}
				return err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return nil
}

// ListASPs returns the application specific passwords issued for one user.
//
// The endpoint is not paginated: it returns the complete set in one response.
func (d *Directory) ListASPs(ctx context.Context, userKey string) ([]*admin.Asp, error) {
	var result *admin.Asps
	err := d.retrier.Do(ctx, "directory.asps.list", func(ctx context.Context) error {
		var callErr error
		result, callErr = d.svc.Asps.List(userKey).Context(ctx).Do()
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("list application specific passwords: %w", err)
	}
	return result.Items, nil
}

// ListTokens returns the third-party OAuth tokens one user has granted.
//
// The endpoint is not paginated.
func (d *Directory) ListTokens(ctx context.Context, userKey string) ([]*admin.Token, error) {
	var result *admin.Tokens
	err := d.retrier.Do(ctx, "directory.tokens.list", func(ctx context.Context) error {
		var callErr error
		result, callErr = d.svc.Tokens.List(userKey).Context(ctx).Do()
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("list third-party tokens: %w", err)
	}
	return result.Items, nil
}

// FullName returns a user's display name, tolerating the absence of the name
// object.
//
// The Name field is a pointer and the API omits it for some accounts (notably
// ones created by directory sync). Earlier versions dereferenced it
// unconditionally, so a single such account crashed the whole report with a nil
// pointer panic.
func FullName(user *admin.User) string {
	if user == nil || user.Name == nil {
		return ""
	}
	if user.Name.FullName != "" {
		return user.Name.FullName
	}
	// Fall back to the components when the composed name is absent.
	switch {
	case user.Name.GivenName != "" && user.Name.FamilyName != "":
		return user.Name.GivenName + " " + user.Name.FamilyName
	case user.Name.DisplayName != "":
		return user.Name.DisplayName
	case user.Name.GivenName != "":
		return user.Name.GivenName
	default:
		return user.Name.FamilyName
	}
}
