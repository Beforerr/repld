install:
    -repld stop
    go build -C go -o ~/.local/bin/repld .
    npx skills add . -g -y

test:
    go test -C go -v -timeout 300s

test-fast:
    go test -C go -run 'Test(Parse|Handle|SessionManager|ExecuteRaw|InterruptUnknown)' -count=1

bench-startup runs="5":
    #!/usr/bin/env bash
    set -euo pipefail
    bin=repld
    socket="$(mktemp -t repld-bench.XXXXXX.sock)"
    rm -f "$socket"
    cleanup() {
        "$bin" --socket "$socket" stop >/dev/null 2>&1 || true
        rm -f "$socket"
    }
    trap cleanup EXIT
    "$bin" --socket "$socket" julia -e 'println(1)' >/dev/null

    hyperfine --runs {{ runs }} \
        --command-name "cold startup + first eval" \
        --prepare "'$bin' --socket '$socket' stop >/dev/null 2>&1 || true; rm -f '$socket'" \
        "'$bin' --socket '$socket' julia -e 'println(1)' >/dev/null"

    hyperfine --runs {{ runs }} --warmup 1 --shell=none \
        --command-name "warm eval" \
        "'$bin' --socket '$socket' julia -e 'println(1)' >/dev/null"

release version="":
    #!/usr/bin/env bash
    set -euo pipefail
    version="{{ version }}"
    if [[ -z "$version" ]]; then
        latest=$(jj log --no-graph -r 'tags()' --template 'tags ++ "\n"' 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)
        if [[ -z "$latest" ]]; then
            echo "No existing semver tag found. Please provide a version explicitly: just release v0.1.0" >&2
            exit 1
        fi
        IFS='.' read -r major minor patch <<< "${latest#v}"
        version="v${major}.$((minor + 1)).0"
        echo "Bumping minor: $latest -> $version"
    fi
    jj tag set "$version" --revision @-
    git push --tags
