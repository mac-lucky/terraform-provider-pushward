package client

import "time"

// The shapes below hold only the fields the provider reads; the API's
// public OpenAPI spec has the rest.

type Org struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Role        string     `json:"role"`
	Plan        string     `json:"plan"`
	Status      string     `json:"status"`
	MemberCount int64      `json:"member_count"`
	MemberLimit int64      `json:"member_limit"`
	Seats       *int64     `json:"seats,omitempty"`
	TrialEndsAt *time.Time `json:"trial_ends_at,omitempty"`
	DeleteAfter *time.Time `json:"delete_after,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Member struct {
	UserID   string    `json:"user_id"`
	Nickname *string   `json:"nickname"`
	Role     string    `json:"role"`
	Email    *string   `json:"email,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
}

type Invite struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"`
	Email      *string    `json:"email,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	RedeemedBy *string    `json:"redeemed_by,omitempty"`
	MaxUses    int64      `json:"max_uses"`
	Uses       int64      `json:"uses"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	MemberIDs   []string  `json:"member_ids"`
	CreatedAt   time.Time `json:"created_at"`
}

type Tag struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	DeviceCount int64     `json:"device_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type Route struct {
	ID            string    `json:"id"`
	Position      int64     `json:"position"`
	SlugPattern   string    `json:"slug_pattern"`
	AppliesTo     string    `json:"applies_to"`
	AllowGroupIDs []string  `json:"allow_group_ids"`
	AllowTagIDs   []string  `json:"allow_tag_ids"`
	Note          *string   `json:"note,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type KeyPermissions struct {
	Activities    string `json:"activities"`
	Notifications string `json:"notifications"`
	Widgets       string `json:"widgets"`
	Emails        string `json:"emails"`
}

type KeyTarget struct {
	GroupIDs []string `json:"group_ids"`
	TagIDs   []string `json:"tag_ids"`
}

// Key is an hlk_ sending key owned by the organization. Secret is set only
// in the responses that create or roll it.
type Key struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Permissions   KeyPermissions `json:"permissions"`
	ActivitySlugs []string       `json:"activity_slugs,omitempty"`
	WidgetSlugs   []string       `json:"widget_slugs,omitempty"`
	Target        *KeyTarget     `json:"target,omitempty"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	Secret        *string        `json:"key,omitempty"`
}

type Device struct {
	UserID                  string    `json:"user_id"`
	Nickname                *string   `json:"nickname"`
	DeviceID                string    `json:"device_id"`
	Name                    *string   `json:"name,omitempty"`
	Platform                *string   `json:"platform,omitempty"`
	DeviceModel             *string   `json:"device_model,omitempty"`
	OSVersion               *string   `json:"os_version,omitempty"`
	AppVersion              *string   `json:"app_version,omitempty"`
	CanReceiveNotifications bool      `json:"can_receive_notifications"`
	CanStartLiveActivities  bool      `json:"can_start_live_activities"`
	Delivery                bool      `json:"delivery"`
	TagIDs                  []string  `json:"tag_ids"`
	CreatedAt               time.Time `json:"created_at"`
}
