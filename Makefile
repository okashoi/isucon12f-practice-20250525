APPNAME := isuconquest.go.service

.PHONY: *
gogo: stop-services build logs/clear start-services bench

stop-services:
	sudo systemctl stop nginx
	sudo systemctl stop $(APPNAME)
	ssh isucon-s2 "sudo systemctl stop $(APPNAME)"
	ssh isucon-s3 "sudo systemctl stop $(APPNAME)"
	ssh isucon-s4 "sudo systemctl stop $(APPNAME)"
	ssh isucon-s5 "sudo systemctl stop $(APPNAME)"
	sudo systemctl stop mysql
	ssh isucon-s2 "sudo systemctl stop mysql"
	ssh isucon-s3 "sudo systemctl stop mysql"
	ssh isucon-s4 "sudo systemctl stop mysql"
	ssh isucon-s5 "sudo systemctl stop mysql"

build:
	cd go && go build -o isuconquest
	scp go/isuconquest isucon-s2:~/webapp/go/isuconquest
	scp go/isuconquest isucon-s3:~/webapp/go/isuconquest
	scp go/isuconquest isucon-s4:~/webapp/go/isuconquest
	scp go/isuconquest isucon-s5:~/webapp/go/isuconquest

logs: limit=100000
logs: opts=
logs:
	journalctl -ex --since "$(shell systemctl status isuconquest.go.service | grep "Active:" | awk '{print $$6, $$7}')" -n $(limit) -q $(opts)

logs/error:
	$(MAKE) logs opts='--grep "status=500" --no-pager'

logs/clear:
	sudo journalctl --vacuum-size=1K
	sudo truncate --size 0 /var/log/nginx/access.log
	sudo truncate --size 0 /var/log/nginx/error.log
	sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log
	sudo truncate --size 0 /var/log/mysql/error.log
	ssh isucon-s2 "sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log"
	ssh isucon-s2 "sudo truncate --size 0 /var/log/mysql/error.log"
	ssh isucon-s3 "sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log"
	ssh isucon-s3 "sudo truncate --size 0 /var/log/mysql/error.log"
	ssh isucon-s4 "sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log"
	ssh isucon-s4 "sudo truncate --size 0 /var/log/mysql/error.log"
	ssh isucon-s5 "sudo truncate --size 0 /var/log/mysql/mysql-slow.log && sudo chmod 666 /var/log/mysql/mysql-slow.log"
	ssh isucon-s5 "sudo truncate --size 0 /var/log/mysql/error.log"

start-services:
	sudo systemctl daemon-reload
	ssh isucon-s2 "sudo systemctl start mysql"
	ssh isucon-s3 "sudo systemctl start mysql"
	ssh isucon-s4 "sudo systemctl start mysql"
	ssh isucon-s5 "sudo systemctl start mysql"
	sudo systemctl start $(APPNAME)
	ssh isucon-s2 "sudo systemctl start $(APPNAME)"
	ssh isucon-s3 "sudo systemctl start $(APPNAME)"
	ssh isucon-s4 "sudo systemctl start $(APPNAME)"
	ssh isucon-s5 "sudo systemctl start $(APPNAME)"
	sudo systemctl start nginx

bench:
	ssh isucon-bench "./bin/benchmarker --stage=prod -target-host=172.31.34.129 --request-timeout=10s"

kataribe: timestamp=$(shell TZ=Asia/Tokyo date "+%Y%m%d-%H%M%S")
kataribe:
	mkdir -p ~/kataribe-logs
	sudo cp /var/log/nginx/access.log /tmp/last-access.log && sudo chmod 0666 /tmp/last-access.log
	cat /tmp/last-access.log | kataribe -conf kataribe.toml > ~/kataribe-logs/$(timestamp).log
	cat ~/kataribe-logs/$(timestamp).log | grep --after-context 20 "Top 20 Sort By Total"

pprof: time=90
pprof: prof_file=/tmp/pprof/pprof.samples.$(shell TZ=Asia/Tokyo date +"%H%M").$(shell git rev-parse HEAD | cut -c 1-8).pb.gz
pprof:
	@mkdir -p /tmp/pprof
	curl -sSf "http://localhost:6060/debug/fgprof?seconds=$(time)" > $(prof_file)
	go tool pprof $(prof_file)
