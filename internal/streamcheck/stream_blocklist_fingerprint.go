package streamcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/javi11/nzbparser"
)

type Identity struct {
	Size       int64
	Poster     string
	UsenetDate int64
}

func ComputeStreamBlocklistFingerprint(size int64, poster string, usenetDate int64) string {
	if size <= 0 {
		return ""
	}
	poster = strings.TrimSpace(strings.ToLower(poster))
	if poster == "" && usenetDate <= 0 {
		return ""
	}
	dayBucket := int64(0)
	if usenetDate > 0 {
		dayBucket = usenetDate / 86400
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d", size, poster, dayBucket)))
	return "wd1:" + hex.EncodeToString(sum[:16])
}

func StreamBlocklistFingerprintFromNZB(n *nzbparser.Nzb, override Identity) string {
	identity := override
	if n == nil {
		return ComputeStreamBlocklistFingerprint(identity.Size, identity.Poster, identity.UsenetDate)
	}
	if identity.Size <= 0 {
		identity.Size = n.Bytes
	}
	for i := range n.Files {
		f := n.Files[i]
		if isPar2(f.Filename) || isPar2(f.Subject) {
			continue
		}
		if identity.Poster == "" {
			identity.Poster = f.Poster
		}
		if identity.UsenetDate == 0 {
			identity.UsenetDate = int64(f.Date)
		}
		if identity.Poster != "" || identity.UsenetDate > 0 {
			break
		}
	}
	if identity.Poster == "" && identity.UsenetDate == 0 {
		for i := range n.Files {
			f := n.Files[i]
			if identity.Poster == "" {
				identity.Poster = f.Poster
			}
			if identity.UsenetDate == 0 {
				identity.UsenetDate = int64(f.Date)
			}
			if identity.Poster != "" || identity.UsenetDate > 0 {
				break
			}
		}
	}
	return ComputeStreamBlocklistFingerprint(identity.Size, identity.Poster, identity.UsenetDate)
}

func StreamBlocklistBackbone(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, ':'); i > 0 {
		h = h[:i]
	}
	if h == "" {
		return "unknown"
	}
	return h
}

func StreamBlocklistRootDomain(host string) string {
	h := strings.Trim(strings.TrimSpace(strings.ToLower(host)), ".")
	if h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, ':'); i > 0 {
		h = h[:i]
	}
	if net.ParseIP(h) != nil {
		return h
	}
	parts := strings.FieldsFunc(h, func(r rune) bool { return r == '.' })
	if len(parts) <= 2 {
		return h
	}
	take := 2
	if len(parts[len(parts)-1]) == 2 && len(parts[len(parts)-2]) <= 3 {
		take = 3
	}
	if len(parts) <= take {
		return h
	}
	return strings.Join(parts[len(parts)-take:], ".")
}
