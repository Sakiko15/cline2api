package main

import "time"

type Account struct {
	AccountID        string    `json:"accountId"`
	Email            string    `json:"email"`
	RefreshToken     string    `json:"refreshToken"`
	AccessToken      string    `json:"-"`
	ExpiresAt        int64     `json:"-"`
	Status           string    `json:"status"` // active, cooldown, expired
	CooldownUntil    time.Time `json:"cooldownUntil,omitempty"`
	LastUsed         time.Time `json:"lastUsed"`
	UsageCount       int64     `json:"usageCount"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	TotalTokens      int64     `json:"totalTokens"`
	CachedTokens     int64     `json:"cachedTokens"`
	CreatedAt        time.Time `json:"createdAt"`
}

type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Cost     string `json:"cost"`   // "free" | "pass"
	Status   string `json:"status"` // "active"
	Custom   bool   `json:"custom"` // true=用户手动添加，可删除
}

type AccountPool struct {
	Accounts     []*Account `json:"accounts"`
	CurrentIdx   int        `json:"currentIdx"`
	Keys         []string   `json:"keys,omitempty"`
	Models       []Model    `json:"models,omitempty"`
	DefaultModel string     `json:"defaultModel,omitempty"`
}

type LoginMethod int

const (
	MethodDeviceOAuth LoginMethod = iota
	MethodRefreshToken
	MethodSSOCookie
)
