package config

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
)

func ExperimentID(name string, scope Scope, seed *uint64, limits Limits) string {
	digest := sha256.New()
	writeField(digest, name)
	writeField(digest, scope.Network)
	for _, namespace := range scope.Namespaces {
		writeField(digest, namespace)
	}
	if seed == nil {
		writeField(digest, "seed:none")
	} else {
		writeField(digest, "seed:"+strconv.FormatUint(*seed, 10))
	}
	writeField(digest, strconv.Itoa(limits.MaxNamespaces))
	writeField(digest, strconv.Itoa(limits.MaxInterfaces))
	writeField(digest, limits.MaxDuration.String())
	writeField(digest, limits.MaxBandwidth.String())
	writeField(digest, strconv.Itoa(limits.MaxPacketRate))
	writeField(digest, strconv.Itoa(limits.MaxConcurrentProbes))
	sum := digest.Sum(nil)
	return "nf-" + name + "-" + hex.EncodeToString(sum[:4])
}

func writeField(digest hash.Hash, value string) {
	_, _ = digest.Write([]byte(strconv.Itoa(len(value))))
	_, _ = digest.Write([]byte(":"))
	_, _ = digest.Write([]byte(value))
	_, _ = digest.Write([]byte{0})
}

func ExperimentIDFor(c Config) string {
	return ExperimentID(c.Name, c.Scope, c.Seed, c.Limits)
}
