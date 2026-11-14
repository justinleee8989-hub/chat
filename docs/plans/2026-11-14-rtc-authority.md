# Server-owned RTC authority, 2026-11-14
User approved completing server room permissions, authoritative state and abnormal disconnect handling, preserving UI/data. No original account mutations.

API: authenticated ChatAPI routes create/action/list/get_token. Caller/callee transitions enforced server side, canonical room UUID generated server side; accepted direct call required for tokens; group rooms require actual group membership for create/join/heartbeat/token. Existing unrestricted RPC token route fails closed. Redis namespace rtc:authority:v1 isolates all new records; optimistic WATCH/MULTI serialization across API instances with bounded retained history. No legacy data migration.

State: ringing (45s), accepted (per-user 35s heartbeat lease), ended/rejected/canceled/missed/disconnected; terminal immutable; repeated same action idempotent. Per-user busy checked atomically. Direct caller can cancel, callee accept/reject, either accepted participant end. Client requests never determine actor or room membership. Group leave affects only self; room ends when no active participants. Poll lists only own calls.

Media: 30-second room token, server-generated per-admission identity bound to the authenticated user (JWT name=userID); a rejoin rotates identity; signed LiveKit participant callback rechecks active authorization and removes unauthorized/terminal participants. Periodic room cleanup retries terminal room deletion beyond token lifetime. Heartbeats alone cannot resurrect terminal calls. Authenticated callback body size bounded. Server outage fails closed.

Verification: pure state unit/race tests, real Redis concurrency tests, authenticated API abuse tests using new accounts, frontend mock controller tests and browser pair acceptance. Low-tier agent handles tests. Native/device/video acceptance excluded.

Cleanup recovery: failed RemoveParticipant tasks persist keyed by the old admission identity, so late concurrent cleanup cannot evict a rejoined member. Failed terminal DeleteRoom tasks are retained beyond the history retention until confirmed cleaned. Webhooks require LiveKit signatures, accept its standard header format, and receive a generated OpenIM trace id. User existence is verified by get_users_info, never registration availability.
