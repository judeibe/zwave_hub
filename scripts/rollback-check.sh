#!/bin/sh
# Rollback drill for constitution VII: a release that migrates the database
# forward must leave the previous release able to serve the same data, and a
# database a newer release wrote must never be downgraded in place.
#
# The script runs the image in the repository's build (IMAGE:TAG) against
# bind-mounted directories, so the host's sqlite3 can read PRAGMA user_version
# between runs. Every leg gets its own copy of the data directory: a directory
# that one container has already mounted is not reliably writable from the next
# one over virtiofs, and a rollback restores a copy anyway.
#
# Legs:
#   1. fresh volume: the binary migrates to the schema it embeds and serves
#      /readyz and the audit trail;
#   2. newer database: a copy with user_version bumped past the build's latest
#      migration must be refused, with the file left untouched — that is the
#      rollback the hub supports: it refuses, it never downgrades in place;
#   2b. back: the forward database serves again unchanged;
#   3. previous image: only when PREVIOUS_IMAGE is set. It runs against a copy
#      of the current database and must serve without touching user_version or
#      the rows it does not know about. Without a tag there is nothing honest
#      to assert, so the leg reports that it was skipped and the caller must
#      not read a pass as a completed rollback drill.
#
# Environment:
#   IMAGE           repository to run (default zwave-hub)
#   TAG             tag to run (default dev)
#   PREVIOUS_IMAGE  full previous image reference, e.g. zwave-hub:0.9.0
#   ROLLBACK_DIR    scratch directory (default: a fresh mktemp -d)
#   PORT            first host port to publish (default 18090; the next two are
#                   used by the other legs)
#
# POSIX sh, no bashisms: it is invoked by `make rollback-check` as
# `sh scripts/rollback-check.sh`.

set -eu

IMAGE_REF="${IMAGE:-zwave-hub}:${TAG:-dev}"
PREVIOUS_IMAGE="${PREVIOUS_IMAGE:-}"
BASE_PORT="${PORT:-18090}"
CONTAINER="${CONTAINER_NAME:-zwave-hub-rollback-check}"

if [ -n "${ROLLBACK_DIR:-}" ]; then
	DIR="$ROLLBACK_DIR"
	mkdir -p "$DIR"
else
	DIR="$(mktemp -d)"
fi

# The hub is non-root in the image. Running the container as the invoking user
# keeps the bind mount writable without a host-side chown.
UID_GID="$(id -u):$(id -g)"

cleanup() {
	docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
trap 'exit 1' HUP

fail() {
	echo "rollback-check: FAIL: $*" >&2
	exit 1
}

# start DATA IMAGE HOST_PORT runs the container detached for wait_ready.
start() {
	cleanup
	docker run -d --name "$CONTAINER" \
		--user "$UID_GID" \
		--mount "type=bind,src=$1,dst=/data" \
		--tmpfs /run/zwave-hub:mode=1777 \
		-p "127.0.0.1:$3:8080" \
		-e ZWAVE_HUB_BIND=0.0.0.0:8080 \
		"$2" >/dev/null
}

# wait_ready PORT polls /readyz for up to 45 s and prints the body once.
wait_ready() {
	tries=0
	while [ "$tries" -lt 45 ]; do
		if body="$(curl -fsS "http://127.0.0.1:$1/readyz" 2>/dev/null)"; then
			echo "  /readyz: $body"
			return 0
		fi
		if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || echo false)" != "true" ]; then
			echo "  container log:" >&2
			docker logs "$CONTAINER" 2>&1 | tail -n 20 >&2 || true
			return 1
		fi
		tries=$((tries + 1))
		sleep 1
	done
	return 1
}

# wait_exit polls until the container stops and prints its exit code. A
# container still running after 30 s never refused the database, which is the
# failure this leg exists to catch, so it is stopped and reported as 0.
wait_exit() {
	tries=0
	while [ "$tries" -lt 30 ]; do
		if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || echo false)" != "true" ]; then
			docker inspect -f '{{.State.ExitCode}}' "$CONTAINER"
			return 0
		fi
		tries=$((tries + 1))
		sleep 1
	done
	docker stop "$CONTAINER" >/dev/null 2>&1 || true
	echo "0"
}

# stop halts the container and folds its WAL into hub.db, so the host's sqlite3
# reads exactly what the next leg will.
stop() {
	docker stop "$CONTAINER" >/dev/null 2>&1 || true
	cleanup
}

# user_version DATA prints the database's PRAGMA user_version. The read opens
# the file in WAL mode, which creates -shm and -wal; they are removed so the
# next container starts from a directory the host has fully settled.
user_version() {
	version="$(sqlite3 "$1/hub.db" 'PRAGMA user_version')"
	sqlite3 "$1/hub.db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null
	rm -f "$1/hub.db-wal" "$1/hub.db-shm"
	echo "$version"
}

# copy_data FROM TO copies a stopped data directory: the database, the master
# key and nothing else.
copy_data() {
	mkdir -p "$2"
	cp "$1/hub.db" "$2/hub.db"
	cp "$1/master.key" "$2/master.key"
}

audit_ok() {
	code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$1/api/v1/audit?limit=5")"
	[ "$code" = "200" ] || fail "GET /api/v1/audit returned $code"
	echo "  GET /api/v1/audit: 200"
}

FORWARD="$DIR/forward"
NEWER="$DIR/newer"
BACK="$DIR/back"
PREVIOUS="$DIR/previous"

echo "rollback-check: image $IMAGE_REF, scratch $DIR"

echo "1/3 fresh volume: migrate forward and serve"
rm -rf "$FORWARD"
mkdir -p "$FORWARD"
start "$FORWARD" "$IMAGE_REF" "$BASE_PORT"
if ! wait_ready "$BASE_PORT"; then
	docker logs "$CONTAINER" >&2 || true
	fail "the image did not reach /readyz on a fresh volume"
fi
stop
FORWARD_VERSION="$(user_version "$FORWARD")"
echo "  PRAGMA user_version: $FORWARD_VERSION"
[ "$FORWARD_VERSION" -ge 1 ] || fail "a fresh database is at user_version $FORWARD_VERSION, want at least 1"
# The audit endpoint answers on a database that was migrated by this very run.
start "$FORWARD" "$IMAGE_REF" "$BASE_PORT"
wait_ready "$BASE_PORT" >/dev/null || fail "the migrated volume did not reach /readyz again"
audit_ok "$BASE_PORT"
stop

echo "2/3 newer database: refuse to downgrade in place"
copy_data "$FORWARD" "$NEWER"
cp "$FORWARD/hub.db" "$DIR/hub.db.forward"
NEWER_VERSION=$((FORWARD_VERSION + 1))
sqlite3 "$NEWER/hub.db" "PRAGMA user_version = $NEWER_VERSION"
sqlite3 "$NEWER/hub.db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null
rm -f "$NEWER/hub.db-wal" "$NEWER/hub.db-shm"
start "$NEWER" "$IMAGE_REF" "$((BASE_PORT + 1))"
EXIT_CODE="$(wait_exit)"
echo "  exit code on user_version $NEWER_VERSION: $EXIT_CODE"
[ "$EXIT_CODE" != "0" ] || fail "the binary accepted a database newer than it supports"
if docker logs "$CONTAINER" 2>&1 | grep -q "newer than this build"; then
	echo "  log names the newer schema"
else
	docker logs "$CONTAINER" 2>&1 | tail -n 5 >&2 || true
	fail "the refusal did not name the newer schema"
fi
cleanup
[ "$(user_version "$NEWER")" = "$NEWER_VERSION" ] || fail "the refused database was modified"
echo "  the refused file is untouched at user_version $NEWER_VERSION"

echo "2b/3 back: the forward database serves again unchanged"
copy_data "$FORWARD" "$BACK"
start "$BACK" "$IMAGE_REF" "$((BASE_PORT + 1))"
if ! wait_ready "$((BASE_PORT + 1))"; then
	docker logs "$CONTAINER" >&2 || true
	fail "the restored database did not reach /readyz"
fi
stop
[ "$(user_version "$BACK")" = "$FORWARD_VERSION" ] || fail "the restored database changed user_version"
start "$BACK" "$IMAGE_REF" "$((BASE_PORT + 1))"
wait_ready "$((BASE_PORT + 1))" >/dev/null || fail "the restored database did not reach /readyz again"
audit_ok "$((BASE_PORT + 1))"
stop
echo "  PRAGMA user_version: $FORWARD_VERSION"

echo "3/3 previous image"
if [ -z "$PREVIOUS_IMAGE" ]; then
	echo "  SKIPPED: PREVIOUS_IMAGE is unset, so no previous release was run."
	echo "  The previous-image leg — the previous binary reading a database the"
	echo "  current one wrote, ignoring columns it does not know — was not executed."
	echo "rollback-check: forward, refusal and back legs passed; previous-image leg NOT run."
	exit 0
fi

docker image inspect "$PREVIOUS_IMAGE" >/dev/null 2>&1 || fail "previous image $PREVIOUS_IMAGE is not present"
copy_data "$FORWARD" "$PREVIOUS"
start "$PREVIOUS" "$PREVIOUS_IMAGE" "$((BASE_PORT + 2))"
if ! wait_ready "$((BASE_PORT + 2))"; then
	docker logs "$CONTAINER" >&2 || true
	fail "the previous image did not reach /readyz against the current database"
fi
audit_ok "$((BASE_PORT + 2))"
stop
[ "$(user_version "$PREVIOUS")" = "$FORWARD_VERSION" ] || fail "the previous image changed user_version"
echo "  PRAGMA user_version after the previous image: $FORWARD_VERSION"

echo "rollback-check: passed (previous image $PREVIOUS_IMAGE on the current database copy)"
