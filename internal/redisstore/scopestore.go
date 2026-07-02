// Cross-replica velocity ($/min, tokens/min) and concurrency enforcement (ADR
// 0002). This makes the firewall's reserve/settle caps hold across replicas, not
// just per instance. The whole multi-scope check-and-reserve is ONE atomic Lua
// EVAL, so concurrent admits across replicas cannot TOCTOU past a cap — the same
// guarantee the local reserve-under-mutex gives, extended cluster-wide.
//
// Layout per scope key `sk`:
//   - heave:vel:usd:<sk>  HASH  field = unix-second, value = reserved+settled $
//   - heave:vel:tok:<sk>  HASH  field = unix-second, value = reserved+settled toks
//   - heave:conc:<sk>     ZSET  member = holdID, score = expiry (now+holdTTL)
//
// The velocity hashes are a rolling window: the sum ignores (and deletes) fields
// older than the window. The concurrency ZSET is a distributed semaphore: a
// crashed replica's holds are reaped when their expiry passes (holdTTL).
//
// Reads/writes FAIL OPEN on a Redis error (an outage must not block all traffic);
// the per-client monthly budget remains the absolute local ceiling.

package redisstore

import (
	"context"
	"fmt"
	"time"
)

const (
	// windowSecs is the velocity window; matches the firewall's local 60s ring.
	windowSecs = 60
	// holdTTLSecs bounds how long a concurrency hold survives without release, so
	// a crashed replica cannot leak a slot forever. Generous vs. request timeout.
	holdTTLSecs = 300
)

// reserveSrc atomically checks every scope's velocity + concurrency and, if
// ALL pass, reserves the estimate in each. KEYS are flat triples
// [velUsd,velTok,conc] per scope. ARGV: now, window, holdTTL, estUSD, estTok,
// holdID, nScopes, then per scope: maxUSD, maxTok, maxInflight, name.
// Returns {1,"",""} on admit or {0,scopeName,kind} on the first breach.
const reserveSrc = `
local now=tonumber(ARGV[1]); local win=tonumber(ARGV[2]); local httl=tonumber(ARGV[3])
local estU=tonumber(ARGV[4]); local estT=tonumber(ARGV[5]); local hold=ARGV[6]; local n=tonumber(ARGV[7])
-- pass 1: check all scopes (only GC mutations: purge stale window fields + reap
-- expired concurrency holds; these are safe even if a later scope then denies)
for s=0,n-1 do
  local ku=KEYS[s*3+1]; local kt=KEYS[s*3+2]; local kc=KEYS[s*3+3]
  local base=8+s*4
  local maxU=tonumber(ARGV[base]); local maxT=tonumber(ARGV[base+1]); local maxC=tonumber(ARGV[base+2]); local name=ARGV[base+3]
  if maxU>0 then
    local sum=0; local all=redis.call('HGETALL',ku)
    for i=1,#all,2 do local sec=tonumber(all[i])
      if now-sec<win then sum=sum+tonumber(all[i+1]) else redis.call('HDEL',ku,all[i]) end
    end
    if sum+estU>maxU then return {0,name,'velocity'} end
  end
  if maxT>0 then
    local sum=0; local all=redis.call('HGETALL',kt)
    for i=1,#all,2 do local sec=tonumber(all[i])
      if now-sec<win then sum=sum+tonumber(all[i+1]) else redis.call('HDEL',kt,all[i]) end
    end
    if sum+estT>maxT then return {0,name,'velocity'} end
  end
  if maxC>0 then
    redis.call('ZREMRANGEBYSCORE',kc,'-inf',now)
    if redis.call('ZCARD',kc)>=maxC then return {0,name,'concurrency'} end
  end
end
-- pass 2: reserve all
for s=0,n-1 do
  local ku=KEYS[s*3+1]; local kt=KEYS[s*3+2]; local kc=KEYS[s*3+3]
  local base=8+s*4
  local maxU=tonumber(ARGV[base]); local maxT=tonumber(ARGV[base+1]); local maxC=tonumber(ARGV[base+2])
  if maxU>0 then redis.call('HINCRBYFLOAT',ku,tostring(now),tostring(estU)); redis.call('EXPIRE',ku,win+1) end
  if maxT>0 then redis.call('HINCRBY',kt,tostring(now),estT); redis.call('EXPIRE',kt,win+1) end
  if maxC>0 then redis.call('ZADD',kc,now+httl,hold); redis.call('EXPIRE',kc,httl+1) end
end
return {1,'',''}
`

// adjustSrc adds a signed delta to the CURRENT second of each scope's velocity
// hashes, flooring each field at 0. Used by Settle (delta = actual-est) and by
// Release of an unsettled reservation (delta = -est). KEYS are flat pairs
// [velUsd,velTok] per scope. ARGV: now, window, deltaUSD, deltaTok, nScopes.
const adjustSrc = `
local now=tonumber(ARGV[1]); local win=tonumber(ARGV[2])
local dU=tonumber(ARGV[3]); local dT=tonumber(ARGV[4]); local n=tonumber(ARGV[5])
for s=0,n-1 do
  local ku=KEYS[s*2+1]; local kt=KEYS[s*2+2]
  if dU~=0 then
    local v=tonumber(redis.call('HINCRBYFLOAT',ku,tostring(now),tostring(dU)))
    if v<0 then redis.call('HSET',ku,tostring(now),'0') end
    redis.call('EXPIRE',ku,win+1)
  end
  if dT~=0 then
    local w=tonumber(redis.call('HINCRBY',kt,tostring(now),dT))
    if w<0 then redis.call('HSET',kt,tostring(now),0) end
    redis.call('EXPIRE',kt,win+1)
  end
end
return 1
`

// releaseConcSrc drops a concurrency hold from each scope's ZSET.
// KEYS = conc keys; ARGV = holdID.
const releaseConcSrc = `
for i=1,#KEYS do redis.call('ZREM',KEYS[i],ARGV[1]) end
return 1
`

func velUsdKey(sk string) string { return "heave:vel:usd:" + sk }
func velTokKey(sk string) string { return "heave:vel:tok:" + sk }
func concKey(sk string) string   { return "heave:conc:" + sk }

// Reserve atomically checks and reserves across all scopes. Parallel slices:
// keys[i] has caps maxUSD[i]/maxTokens[i]/maxInflight[i] and display name
// names[i] (0 caps are skipped). It returns admitted, and on denial the scope
// name + kind ("velocity"|"concurrency"). holdID identifies the concurrency
// holds for Release. FAILS OPEN: on a Redis error it returns admitted=true with a
// non-nil error so the caller can record the degradation.
func (s *Store) Reserve(keys, names []string, maxUSD []float64, maxTokens, maxInflight []int, estUSD float64, estTokens int, holdID string) (admitted bool, deniedName, deniedKind string, err error) {
	n := len(keys)
	redisKeys := make([]string, 0, n*3)
	for _, sk := range keys {
		redisKeys = append(redisKeys, velUsdKey(sk), velTokKey(sk), concKey(sk))
	}
	args := []any{s.nowUnix(), windowSecs, s.holdTTL, estUSD, estTokens, holdID, n}
	for i := range keys {
		args = append(args, maxUSD[i], maxTokens[i], maxInflight[i], names[i])
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	res, rerr := s.reserveScript.Run(ctx, s.rdb, redisKeys, args...).Slice()
	if rerr != nil {
		// Fail open: enforcement degrades to unenforced, never blocks all traffic.
		return true, "", "", fmt.Errorf("redis scope reserve: %w", rerr)
	}
	if len(res) >= 1 {
		if code, _ := res[0].(int64); code == 1 {
			return true, "", "", nil
		}
	}
	name, _ := res[1].(string)
	kind, _ := res[2].(string)
	return false, name, kind, nil
}

// Settle reconciles a reservation to actual spend (delta into the current window
// bucket of every scope). Best-effort; a Redis error is returned but non-fatal.
func (s *Store) Settle(keys []string, deltaUSD float64, deltaTokens int) error {
	return s.adjust(keys, deltaUSD, deltaTokens)
}

// Release frees the concurrency holds for holdID across all scopes and, if the
// request never settled, removes the reserved estimate from the windows.
func (s *Store) Release(keys []string, holdID string, estUSD float64, estTokens int, settled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	concKeys := make([]string, len(keys))
	for i, sk := range keys {
		concKeys[i] = concKey(sk)
	}
	if err := s.releaseConcScript.Run(ctx, s.rdb, concKeys, holdID).Err(); err != nil {
		return fmt.Errorf("redis scope release (conc): %w", err)
	}
	if settled {
		return nil
	}
	return s.adjust(keys, -estUSD, -estTokens)
}

func (s *Store) adjust(keys []string, deltaUSD float64, deltaTokens int) error {
	if deltaUSD == 0 && deltaTokens == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	redisKeys := make([]string, 0, len(keys)*2)
	for _, sk := range keys {
		redisKeys = append(redisKeys, velUsdKey(sk), velTokKey(sk))
	}
	args := []any{s.nowUnix(), windowSecs, deltaUSD, deltaTokens, len(keys)}
	if err := s.adjustScript.Run(ctx, s.rdb, redisKeys, args...).Err(); err != nil {
		return fmt.Errorf("redis scope adjust: %w", err)
	}
	return nil
}

// nowUnix is the store's clock (injectable for tests via SetClock).
func (s *Store) nowUnix() int64 {
	if s.now != nil {
		return s.now()
	}
	return time.Now().Unix()
}
