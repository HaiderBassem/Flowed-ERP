package app

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"flowed/internal/adapter/receipt"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// Setting keys. Named constants rather than string literals at the call sites,
// because a typo in a key is not an error — it is a setting that silently reads
// as empty and a receipt that silently loses its letterhead.
const (
	SettingUniversityName = "university_name_ar"
	SettingCollegeName    = "college_name_ar"
	SettingAddress        = "address"
	SettingPhone          = "phone"
	SettingLogo           = "logo_data_uri"
	SettingReceiptFooter  = "receipt_footer_ar"
	SettingCurrencyName   = "currency_name_ar"
)

// EditableSettings is every key the settings screen may write.
//
// An allow-list rather than "whatever the client sent". The table upserts on
// write, so without this any client could invent keys and the table would
// slowly fill with rows nothing reads.
var EditableSettings = []string{
	SettingUniversityName,
	SettingCollegeName,
	SettingAddress,
	SettingPhone,
	SettingLogo,
	SettingReceiptFooter,
	SettingCurrencyName,
}

// SettingsService reads and writes the institution's details.
//
// It caches. Every receipt render and every report header asks for the whole
// letterhead, and a university's name changes about once a decade — a query per
// render would be a round trip to answer a question whose answer is the same
// all day. The cache is dropped by the only thing that can change it, Update.
type SettingsService struct {
	deps  Deps
	store port.SettingsRepository
	// fallback is what configuration supplied at start-up, used only for a key
	// the database has no row for. That happens on a deployment which upgraded
	// past migration 000033 with the environment variables still set: their
	// receipts keep printing what they printed yesterday.
	fallback receipt.Institution

	mu     sync.RWMutex
	cached map[string]string
	auditor
}

// NewSettingsService wires the settings commands.
func NewSettingsService(
	d Deps, store port.SettingsRepository, fallback receipt.Institution,
) *SettingsService {
	return &SettingsService{
		deps:     d,
		store:    store,
		fallback: fallback,
		auditor:  newAuditor(d.Audit, d.Clock),
	}
}

// All returns every setting, reading through the cache.
func (s *SettingsService) All(ctx context.Context) (map[string]string, error) {
	s.mu.RLock()
	cached := s.cached
	s.mu.RUnlock()
	if cached != nil {
		return copyOf(cached), nil
	}

	loaded, err := s.store.All(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached = loaded
	s.mu.Unlock()
	return copyOf(loaded), nil
}

// Institution renders the settings as the receipt layer's letterhead.
//
// A failure here returns the configured fallback rather than an error. A
// database hiccup must not stop a student being handed a receipt for money
// already taken: the worst case is a receipt carrying the name from the
// environment, which is the name it carried before this table existed.
func (s *SettingsService) Institution(ctx context.Context) receipt.Institution {
	values, err := s.All(ctx)
	if err != nil {
		if s.deps.Log != nil {
			s.deps.Log.Warn("reading institution settings; printing the configured fallback",
				"error", err.Error())
		}
		return s.fallback
	}

	pick := func(key, fallback string) string {
		if v := strings.TrimSpace(values[key]); v != "" {
			return v
		}
		return fallback
	}

	return receipt.Institution{
		UniversityNameAr: pick(SettingUniversityName, s.fallback.UniversityNameAr),
		CollegeNameAr:    pick(SettingCollegeName, s.fallback.CollegeNameAr),
		Address:          pick(SettingAddress, s.fallback.Address),
		Phone:            pick(SettingPhone, s.fallback.Phone),
		LogoDataURI:      pick(SettingLogo, s.fallback.LogoDataURI),
		FooterAr:         values[SettingReceiptFooter],
		CurrencyNameAr:   pick(SettingCurrencyName, "دينار عراقي"),
	}
}

// Update writes the settings the screen submitted.
//
// Only keys in EditableSettings are written, and an unknown key is refused
// rather than dropped: a client sending "univercity_name_ar" has a bug, and
// accepting it silently leaves somebody wondering why the name they typed never
// reached a receipt.
func (s *SettingsService) Update(
	ctx context.Context, actor shared.Actor, values map[string]string,
) (map[string]string, error) {
	allowed := map[string]bool{}
	for _, key := range EditableSettings {
		allowed[key] = true
	}

	clean := make(map[string]string, len(values))
	for key, value := range values {
		if !allowed[key] {
			return nil, shared.Validation("settings.unknown_key",
				"%q is not a setting this system holds", key).
				WithDetail("key", key).
				WithDetail("known_keys", EditableSettings)
		}
		if key == SettingLogo {
			if err := validateLogo(value); err != nil {
				return nil, err
			}
		}
		clean[key] = strings.TrimSpace(value)
	}

	before, err := s.All(ctx)
	if err != nil {
		return nil, err
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.store.Set(ctx, clean, actor.UserID); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "app_setting",
			Action:     "settings.updated",
			Actor:      actor,
			Before:     redactLogo(before),
			After:      redactLogo(merge(before, clean)),
			OccurredAt: nowOr(s.deps.Clock),
		})
	})
	if err != nil {
		return nil, err
	}

	// Dropped rather than patched: the next read reloads from the rows that
	// actually committed, so the cache cannot disagree with the database over a
	// write that partly failed.
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()

	return s.All(ctx)
}

// maxLogoBytes bounds the inline logo.
//
// It is embedded in every receipt, so a three-megabyte photograph would be
// three megabytes per print and a thermal printer that appears to have hung.
// Half a megabyte is generous for a crest at print resolution.
const maxLogoBytes = 512 * 1024

func validateLogo(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !strings.HasPrefix(value, "data:image/") {
		return shared.Validation("settings.logo_not_inline",
			"the logo must be an inline data URI; a linked image prints blank on a machine "+
				"with no network").
			WithDetail("remedy", "upload the image through the settings screen rather than pasting a link")
	}
	if len(value) > maxLogoBytes {
		return shared.Validation("settings.logo_too_large",
			"the logo is %d KB and the limit is %d KB, because it is embedded in every receipt",
			len(value)/1024, maxLogoBytes/1024)
	}
	return nil
}

// redactLogo keeps a base64 image out of the audit trail.
//
// The entry exists to say the letterhead changed and who changed it. Storing
// half a megabyte of base64 twice — before and after — per edit would bloat the
// one table that has to stay cheap to verify.
func redactLogo(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		if key == SettingLogo && value != "" {
			out[key] = fmt.Sprintf("<image, %d bytes>", len(value))
			continue
		}
		out[key] = value
	}
	return out
}

func merge(base, over map[string]string) map[string]string {
	out := copyOf(base)
	for key, value := range over {
		out[key] = value
	}
	return out
}

func copyOf(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
