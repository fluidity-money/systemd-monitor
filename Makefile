PREFIX ?= /usr/local
SYSTEMD_DIR ?= /usr/lib/systemd/system
GO ?= go

.PHONY: build cli install test clean
.DELETE_ON_ERROR:

FILES_GO := $(wildcard *.go) go.mod
CLIS := systemd-monitor
UNITS := systemd-monitor.service

build: cli

cli: ${CLIS}

systemd-monitor: ${FILES_GO}
	@${GO} build -o $@ .

install: cli
	@mkdir -p ${DESTDIR}${PREFIX}/bin ${DESTDIR}${SYSTEMD_DIR}
	@install -m755 ${CLIS} ${DESTDIR}${PREFIX}/bin/
	@install -m644 ${UNITS} ${DESTDIR}${SYSTEMD_DIR}/

test:
	@${GO} test -race ./...

clean:
	@rm -f ${CLIS}
