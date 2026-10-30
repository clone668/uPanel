package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestParseGPTRejectsEntriesBeyondReadBuffer(t *testing.T) {
	raw := buildGPT(1000, [][2]uint64{{100, 199}}, func(hdr, _ []byte) {
		binary.LittleEndian.PutUint32(hdr[80:84], 256)
	})
	if _, err := parsePartitionTable(raw); err == nil {
		t.Fatal("truncated GPT entry table must not produce free regions")
	}
}

func TestParseGPTRejectsOverflowingEntryOffset(t *testing.T) {
	raw := buildGPT(1000, nil, func(hdr, _ []byte) {
		binary.LittleEndian.PutUint64(hdr[72:80], math.MaxUint64)
	})
	if _, err := parsePartitionTable(raw); err == nil {
		t.Fatal("an overflowing GPT entry-table offset must return an error, not panic")
	}
}

func TestMemoEnvPreservesDependencyProber(t *testing.T) {
	env, _, _ := testEnv()
	memo := newMemoEnv(context.Background(), env)
	if memo.asEnv().Dependents == nil {
		t.Fatal("capability assessment must use the configured dependency prober")
	}
}

func TestAssessDeleteChecksDependencies(t *testing.T) {
	env, _, _ := testEnv()
	env.Dependents = func(context.Context, DependencyQuery) ([]VolumeDependency, error) {
		return []VolumeDependency{{Kind: "share", ID: "shr_1", Name: "team"}}, nil
	}
	a, err := Assess(context.Background(), formatTopo(t), ActionPartitionDelete,
		PartitionDeleteParams{DeviceID: "sdb1"}, env)
	if err != nil {
		t.Fatal(err)
	}
	mustBlock(t, a, NoticeVolumeInUseByShare)
}

func TestAssessDeleteFailsClosedWhenDependencyQueryFails(t *testing.T) {
	env, _, _ := testEnv()
	env.Dependents = func(context.Context, DependencyQuery) ([]VolumeDependency, error) {
		return nil, errors.New("registry unavailable")
	}
	a, err := Assess(context.Background(), formatTopo(t), ActionPartitionDelete,
		PartitionDeleteParams{DeviceID: "sdb1"}, env)
	if err != nil {
		t.Fatal(err)
	}
	mustBlock(t, a, NoticeVolumeDependenciesUnknown)
}
