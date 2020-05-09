package protocol

import "fmt"

// VersionNumber is a version number as int
type VersionNumber int

// The version numbers, making grepping easier
const (
	Version37 VersionNumber = 37 + iota
	Version38
	Version39
	VersionTLS         VersionNumber = 101
	VersionWhatever    VersionNumber = 0 // for when the version doesn't matter
	VersionUnsupported VersionNumber = -1
	VersionUnknown     VersionNumber = -2
	VersionMP          VersionNumber = 512
)

// SupportedVersions lists the versions that the server supports
// must be in sorted descending order
var SupportedVersions = []VersionNumber{
	VersionMP,
	Version39,
	Version38,
	Version37,
}

// UsesTLS says if this QUIC version uses TLS 1.3 for the handshake
func (vn VersionNumber) UsesTLS() bool {
	return vn == VersionTLS
}

func (vn VersionNumber) String() string {
	switch vn {
	case VersionWhatever:
		return "whatever"
	case VersionUnsupported:
		return "unsupported"
	case VersionUnknown:
		return "unknown"
	case VersionTLS:
		return "TLS dev version (WIP)"
	default:
		return fmt.Sprintf("%d", vn)
	}
}

// VersionNumberToTag maps version numbers ('32') to tags ('Q032')
func VersionNumberToTag(vn VersionNumber) uint32 {
	v := uint32(vn)
	return 'Q' + ((v/100%10)+'0')<<8 + ((v/10%10)+'0')<<16 + ((v%10)+'0')<<24
}

// VersionTagToNumber is built from VersionNumberToTag in init()
func VersionTagToNumber(v uint32) VersionNumber {
	return VersionNumber(((v>>8)&0xff-'0')*100 + ((v>>16)&0xff-'0')*10 + ((v>>24)&0xff - '0'))
}

// IsSupportedVersion returns true if the server supports this version
func IsSupportedVersion(supported []VersionNumber, v VersionNumber) bool {
	for _, t := range supported {
		if t == v {
			return true
		}
	}
	return false
}

// ChooseSupportedVersion finds the best version in the overlap of ours and theirs
// ours is a slice of versions that we support, sorted by our preference (descending)
// theirs is a slice of versions offered by the peer. The order does not matter
// if no suitable version is found, it returns VersionUnsupported
func ChooseSupportedVersion(ours, theirs []VersionNumber) VersionNumber {
	for _, ourVer := range ours {
		for _, theirVer := range theirs {
			if ourVer == theirVer {
				return ourVer
			}
		}
	}
	return VersionUnsupported
}
