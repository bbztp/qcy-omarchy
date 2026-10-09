PLUGIN_ID := io.github.bbztp.qcy

.PHONY: build vet install uninstall clean

build:
	cd backend && go build -o qcyctl .

vet:
	cd backend && go vet ./...

install: build
	./scripts/install.sh --no-build

uninstall:
	rm -f "$(HOME)/.local/bin/qcyctl"
	rm -rf "$(HOME)/.config/omarchy/plugins/$(PLUGIN_ID)"
	@command -v omarchy-shell >/dev/null 2>&1 && omarchy-shell -q shell rescanPlugins || true

clean:
	rm -f backend/qcyctl
