# ensemble — build, ui, test, vet, dev, clean (S-skeleton §6).
# `build` does NOT force a UI rebuild: the committed web/dist placeholder makes
# the go:embed compile, and `ui` is a separate target. `check` = vet + test +
# gofmt.

.PHONY: build ui test vet check dev e2e clean

build:
	go build -o ensemble ./cmd/ensemble

ui:
	cd web && npm install && npm run build

test:
	go test ./...

vet:
	go vet ./...

check: vet test
	@out="$$(gofmt -l . 2>/dev/null)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needs to run on:"; echo "$$out"; exit 1; \
	fi

dev:
	./scripts/dev2.sh

e2e: build
	./scripts/e2e.sh

clean:
	rm -f ensemble coverage.out
	rm -rf web/dist
	mkdir -p web/dist
	git checkout -- web/dist/index.html 2>/dev/null || true
