# Phase A 외부 테스트 준비 검토 (2026-10-25)

상태: **준비 검토만 완료. 서버 생성·배포·포트 개방·유료 자원 사용은 미승인.**
기준 브랜치: `experiment/phase-a-relay` @ `0c762aaf7a3c509bb04b53ab750b57d27946f45b`
(WithAllowLimitedConn 교체 포함). 운영 `dain-svc`·`dain-node`·`llama-server`·크레딧 장부는
이 검토에서 건드리지 않았다.

## 1. Oracle 무료 자원 — 계정 상태를 이 자리에서 확인할 수 없음

이 VM에는 Oracle 계정 접근 수단이 없고, 저장소의 `scripts/oracle-setup.sh`에도
인스턴스 shape·테넌시 사용량이 기록돼 있지 않다. 따라서 **현재 테넌시에 무료 여유가
있는지는 추측하지 않고, 아래 콘솔 확인 절차로만 판정한다.**

공개 가이드 기준(2026-09~10월, Oracle 공식 문서 인용 가이드들 — 콘솔에서 재확인 필수):
Always Free는 Ampere A1 **총 2 OCPU / 12 GB RAM**(2026-06 이후 4 OCPU/24 GB에서 축소),
AMD micro `VM.Standard.E2.1.Micro` **최대 2대**(1/8 OCPU·1 GB RAM), 블록 스토리지 총
200 GB, 월 10 TB egress. A1은 한 리전에 고정되고, 유휴 인스턴스는 회수될 수 있다.

콘솔 확인 절차 (사용자가 직접, cloud.oracle.com):
1. 상단 Free Tier 배너·Billing > Cost Analysis에서 Always Free 사용량 확인.
2. Compute > Instances에서 현재 인스턴스 목록 확인: 운영 VM의 shape·OCPU·RAM·리전 기록.
3. 새 인스턴스 생성 화면에서 shape 선택 시 `VM.Standard.A1.Flex`(Arm) 또는
   `VM.Standard.E2.1.Micro`(AMD) 옆의 **Always Free eligible 표시**를 확인한다.
   표시가 없거나 OCPU/RAM 합계가 한도를 넘으면 무료 생성이 아니다.
4. Block Volume 총합이 200 GB를 넘지 않는지 확인한다 (부트 볼륨 포함).
5. VCN·서브넷·보안 목록에서 443 인바운드 규칙이 **아직 없는 상태**인지 확인한다
   (이번 단계에서는 규칙을 추가하지 않는다).
6. 결과 기록: 운영 VM shape / A1 잔여 OCPU·RAM / Micro 잔여 대수 / 스토리지 잔여 / 리전.

판정 기준:
- **무료 가능**: A1 잔여가 1 OCPU/6 GB 이상이거나 Micro 1대 여유가 있고, 스토리지 잔여가
  50 GB 이상이며, 동일 리전에 생성 가능할 때. Relay는 1/8 OCPU·1 GB Micro로도 충분하다.
- **무료 불가**: 잔여가 0이면 유료 VM을 쓰지 말고 중단한다. A1 용량 부족(Out of capacity)
  에러가 나면 리전을 바꿀 수 없으므로(홈 리전 고정) Micro로 대체하거나 테스트를 미룬다.

주의: $300 체험 크레딧(30일)이 남아 있어도 Always Free가 아닌 자원은 크레딧 소진 후
과금되므로, 생성 화면에서 반드시 Always Free eligible 표시를 확인할 것.

## 2. 도메인 + 신뢰 TLS 인증서 — $0 가능, 단 도메인 확보가 선행 조건

요건: Mac(공공 와이파이, 443만 허용될 수 있음)이 신뢰하는 인증서. 자체서명 인증서는
이 테스트에서 **불가**(Mac이 WSS 연결을 거부하거나 경고 우회가 필요해짐).

옵션 A (권장): 기존에 관리 중인 도메인이 있으면 그 서브도메인 사용
- 예: `relay.<보유 도메인>` — Cloudflare DNS(무료)라면 DNS-01 챌린지로 발급 가능.
- 발급: Let's Encrypt(무료, 90일, 자동 갱신) + certbot 또는 acme.sh.
- 비용: 인증서 $0, 도메인은 이미 보유 중이면 추가 $0.
- **확인 필요: 사용자가 관리 중인 도메인이 있는지 아직 확인되지 않았다.**
  `myhub.myhub-app.workers.dev`는 Workers용 서브도메인이라 외부 VM의 WSS 호스트명으로
  쓸 수 없다.

옵션 B: 보유 도메인이 없으면 무료 서브도메인
- DuckDNS(`<이름>.duckdns.org`) 또는 deSEC — 무료, Public Suffix List 등재로
  Let's Encrypt 발급이 가능하다(검색 가이드 기준). DNS-01 챌린지를 쓰면 포트 80을
  열지 않고도 발급할 수 있고, HTTP-01을 쓰면 발급 시점에 포트 80 개방이 필요하다.
- 단점: 타인에게 노출되는 이름이 무료 DDNS 도메인이라는 점, 서비스 지속성이 제공자
  정책에 달려 있다는 점. 테스트용으로는 충분하고, 운영 전환 시에는 자체 도메인으로
  교체하는 것이 맞다.
- 비용: $0.

옵션 C (대안, 이번 테스트의 주 경로가 아님): Cloudflare Tunnel
- `cloudflared`를 릴레이 VM에 두고 443을 열지 않고 Cloudflare 경유로 노출하는 방식.
  무료 티어로 가능하지만, 테스트하는 경로(WSS 직결)가 바뀌고 Cloudflare 설정 변경이
  필요하므로, WSS 직결 테스트가 목적이면 선택하지 않는다.

결론: 인증서 자체는 $0으로 해결 가능하고, 남은 결정은 **보유 도메인 사용 vs 무료
서브도메인 신규 발급**이다.

## 3. Relay 코드 검토 — 접근 제한·통신량·개인키·재연결

검토 범위: `node/cmd/relay/main.go`, `node/internal/p2p/relay.go`,
`node/internal/p2p/p2p.go`(AutoRelay·`ConnectWithRetry`), `node/internal/protocol/resource.go`.

### 접근 제한 — 대체로 양호, 단 2가지 운영 함정
- 양호: `AllowlistACL`이 예약(`AllowReserve`)과 회선 연결(`AllowConnect`, 송·수신 양쪽
  모두 allowlist)을 Peer ID 기준으로 제한한다. V2 수신자는 payload의 `peer_id`가
  스트림의 인증된 `RemotePeer`와 일치할 때만 ACK `ok`를 반환하고, 불일치는
  `peer_id_mismatch`로 거부한다(로컬 테스트에서 위조 거부 확인).
- 함정 1 (2026-10-25 보안 수정으로 해소): `--allow`를 비우면 이전에는
  **allow-all**이었다. 이제 빈 allowlist는 릴레이 기동을 거부한다
  (default deny). allow-all은 `--allow-all` 명시 옵트인으로만 가능하고
  로컬 테스트 전용이다. 공용 릴레이에서는 반드시 Mac·테스트 노드(브리지
  포함 시 브리지) Peer ID를 넣어야 한다. Peer ID는 노드 첫 실행 시
  `~/.distributed-ai-node/identity.key`에서 생성·고정되므로, **릴레이
  기동 전에 Mac과 테스트 노드를 한 번씩 실행해 Peer ID를 먼저 수집**해야
  한다(순서 주의).
- 함정 2: allowlist는 기동 시 정적이다. 멤버 제외·교체는 릴레이 재시작이
  필요하다. 동적 해지(CRL) 기능은 없다. 철회 절차는 `--allow-file`에서
  해당 ID를 삭제하고 릴레이를 재시작하는 것이다
  (`docs/security-architecture.md` §5). 테스트 기간에는 허용 인원이
  2~3개라 문제없다.

### 통신량 제한 — 테스트 규모에는 충분, 집계·경보는 없음
- 기본값(`defaultRelayResources`, 2026-10-25 보안 수정): 예약 TTL 1시간,
  회선당 1시간/**16 MiB**, 최대 예약 512개, 최대 회선 32개, 회선당 버퍼
  4096, Peer당 최대 예약 4개, IP당 최대 예약 64개. 구조적 전체 상한은
  32회선 × 16 MiB = 512 MiB다.
- 테스트 규모(노드 2대, ResourceInfo 수 KB)에는 넉넉하다. 32개 회선 한도는 대규모
  운영에는 낮지만 이번 테스트에는 해당 없다.
- 문제점: **집계 대역폭 상한·Peer별 사용량 기록·경보가 없다.** 구조적
  상한(512 MiB)이 있어 이전의 32 GiB 소진 경로는 막혔지만, 사용량 미터·경보는
  여전히 없다. 테스트 기간에는 allowlist + 소규모로 통제하고, 릴레이 로그·VM
  네트워크 사용량을 사람이 확인한다.
- 해결됨 (2026-10-25 보안 수정): ResourceInfo 메시지 64 KiB 상한(V1/V2),
  ACK 4 KiB 상한, Peer별 10개/분 속도 제한이 구현·테스트됐다
  (`node/internal/p2p/security_test.go`).

### 개인키 보관 — 양호, 단 TLS 키는 운영자 책임
- 양호: 릴레이 신원키 `identity.key`는 디렉터리 0700·파일 0600으로 저장된다
  (`loadOrCreateKey`). 노드 신원키도 동일하게 0700/0600이다.
- 주의: TLS 인증서 개인키는 `dain-relay --tls-cert/--tls-key`가 읽는 운영자 파일이다.
  코드가 권한을 강제하지 않으므로, certbot 기본 경로(`/etc/letsencrypt/...`, root 전용)
  그대로 쓰거나 복사 시 0600을 유지해야 한다. 키가 유출되면 해당 도메인 인증서를
  폐기·재발급해야 한다.
- 주의: `identity.key`를 잃으면 릴레이 Peer ID가 바뀌어 모든 노드의 `--relay` 주소가
  무효가 된다. 키 파일 백업은 암호화된 별도 보관으로만 하고, 평문 공유·저장소 업로드는
  금지한다.

### 재연결 — 로컬에서 검증됨, WAN 미검증
- 양호: `ConnectWithRetry`는 1초부터 지수 백오프(호출자 상한까지)로 재시도한다.
  AutoRelay는 단일 지정 릴레이용으로 조정돼 있다(boot 5초, 최소 간격 10초, 백오프
  10초, 후보 1개). 로컬 스모크 테스트에서 릴레이 재시작 후 `NO_RESERVATION`을 거쳐
  6번째 시도에 재연결·ACK이 확인됐다.
- 미검증: WAN·실제 NAT에서의 DCUtR 직접 연결 업그레이드, 443 전용 egress,
  공용 와이파이 캡티브 포털 환경. 이번 테스트가 바로 그것을 확인하는 자리다.
- 함정: `dain-relay` CLI에는 `--announce` 플래그가 없다. `RelayConfig.AnnounceAddrs`는
  코드에만 있고 CLI에서 설정할 수 없어서, 릴레이 기동 로그의 주소는 `0.0.0.0` 형태다.
  노드에 줄 WSS 주소(`/dns4/<도메인>/tcp/443/wss/p2p/<릴레이 Peer ID>`)는 운영자가
  도메인+Peer ID로 **직접 조립**해야 한다. 주소 조립 실수가 가장 흔한 실패 원인이 될
  수 있으니, 기동 로그의 Peer ID와 DNS 해석 결과를 대조하는 단계를 테스트 절차에 넣었다.
- 없음: 릴레이용 systemd 유닛·자동 재시작·로그 로테이션·헬스체크가 저장소에 없다.
  운영 VM에서는 `systemd --user` 서비스로 등록하고 `Restart=on-failure`를 넣어야 한다
  (배포 시 별도 작업).

## 4. 최소 테스트 절차 — Mac(서울 공공 와이파이) ↔ Relay ↔ 테스트 노드

전제: 릴레이 VM·도메인·인증서가 준비되고, 사용자가 배포를 승인한 뒤에 수행한다.
이 절차 자체는 아직 실행하지 않는다. Oracle 운영 노드·서비스는 테스트 피어로 쓰지 않는다.

### 준비 (실내/사전)
1. Mac에는 개발 도구를 설치하지 않는다. Linux VM에서 크로스 컴파일한 사전 빌드
   바이너리 `dain-node-darwin-arm64`만 Mac으로 전송해 실행한다(제품 설계 원칙 1).
   출처: `experiment/phase-a-relay` @ `0c762aaf`, `CGO_ENABLED=0 go build -trimpath
   -ldflags "-s -w"`, Go 1.26.0, 24,144,562 bytes, SHA-256
   `d485a94bc0198a36d6ca1994dd9ed1f88f3448ed85eb06f704735bf60ccd8ea6`
   (보관: `~/workspace/dain-prebuilt/` + `SHA256SUMS.txt`). Mac에서 전송 후
   `shasum -a 256 dain-node-darwin-arm64`으로 해시를 대조하고, `chmod +x` 후
   실행한다. **v0.10-alpha 릴리스 바이너리는 --relay가 없어서 쓸 수 없다.**
   (정정 2026-10-25: 초안의 "Mac에서 Go로 직접 빌드" 경로는 제품 설계 원칙 1에
   어긋나므로 사전 빌드 방식으로 교체했다.)
2. Mac에서 노드를 한 번 실행해 Peer ID를 기록한다(안전 모드, 릴레이 없이도 Peer ID
   출력됨). 테스트 노드(로컬 VM 또는 사용자 PC의 별도 노드)도 같은 방식으로 Peer ID를
   기록한다.
3. 릴레이 `--allow`에 Mac Peer ID + 테스트 노드 Peer ID를 넣는다. 빈 allowlist 금지.
4. 릴레이 기동 후 로그에서 릴레이 Peer ID를 확인하고, WSS 주소를 조립한다.
   `/dns4/<도메인>/tcp/443/wss/p2p/<릴레이 Peer ID>`
5. `nslookup <도메인>`이 릴레이 VM 공인 IP를 가리키는지, 인증서 SAN에 도메인이
   들어 있는지 확인한다.

### 현장 (서울 공공 와이파이, Mac)
6. 공공 와이파이에 연결하고 캡티브 포털 인증을 마친다. `curl -v https://<도메인>:443/`
   수준으로 443 egress가 열리는지 먼저 확인한다(릴레이는 HTTP를 안 하므로 연결 자체만
   확인; 실패 시 해당 와이파이는 테스트 불가로 기록).
7. Mac 노드를 안전 모드로 실행한다.
   `./dain-node --cpu 0 --ram 0 --storage 0 --relay /dns4/<도메인>/tcp/443/wss/p2p/<릴레이 Peer ID>`
   `--api`는 지정하지 않는다(운영 서비스 등록을 하지 않는다).
8. 로그 확인: `Relay configured: ... (AutoRelay + hole punching enabled)`, 예약 성공
   여부, 자신의 circuit 주소(`/p2p-circuit` 포함)가 뜨는지 확인한다.
9. 테스트 노드도 같은 릴레이로 실행한다(장소는 실내 유선/일반 와이파이로 충분).
10. Mac에서 테스트 노드의 circuit 주소로 `--connect`를 실행한다.
    `./dain-node --relay <WSS 주소> --connect /p2p/<테스트 노드 Peer ID>` 형태가 아니라,
    실행 중인 노드의 `--connect` 경로를 사용한다(정확한 CLI 형태는 노드 `--help`로
    현장에서 확인). 양방향으로 각 1회씩 교환한다.
11. 성공 판정(전부 충족해야 PASS):
    - 양쪽 모두 예약 성공, `kind=relayed`로 분류됨.
    - 양방향 ResourceInfo 수신 + V2 ACK `ok=true`, `acked=true fallback=false`.
    - 수신 측 로그의 Peer ID가 송신 측 Peer ID와 일치.
    - 허용되지 않은 제3의 Peer ID로는 예약이 거부됨(`PERMISSION_DENIED`).
    - 릴레이 재시작 후 재예약·재연결이 되는지(가능하면 현장에서 1회).
    - direct로 오분류된 relayed 성공이 없을 것.
12. 실패 시 기록: 와이파이 SSID·장소, 캡티브 포털 유무, 443 egress 결과, 릴레이 로그,
    노드 로그 전문, DNS 해석 결과. 추측으로 원인을 단정하지 않는다.
13. 종료 후 Mac 노드와 테스트 노드를 중지하고, 릴레이는 승인된 기간까지만 유지한다.

### 범위 밖 (이번 테스트에서 하지 않음)
- Oracle 운영 `dain-svc`/`dain-node`/`llama-server`에 Mac을 등록·연결하는 것.
- HTTP API 등록(Option B 브리지) — 별도 프로토타입·승인 필요.
- 공동 추론(Phase B) — P2P 연결이 증명된 뒤의 단계.
- 대용량 청크 전송 — 회선 한도(16 MiB) 내라도 이번 테스트 범위가 아님.

## 5. 임시 네트워크 주소 — 남아 있음, 정리 필요

2026-10-25 테스트에서 이 VM의 `lo`에 추가한 `45.33.10.20/32`가 **지금도 남아 있다.**
외부 영향은 없다(로컬 루프백 별칭, AutoRelay 광고 테스트용 공인 형태 주소).
다만 실험용 임시 주소이므로, 다음 로컬 테스트 전에 정리하는 것이 맞다.
정리 명령: `ip addr del 45.33.10.20/32 dev lo` (root 권한).
이 검토에서는 보고만 하고 삭제하지 않았다.

## 6. 예상 비용

| 항목 | 예상 비용 | 비고 |
|---|---|---|
| 테스트 Relay VM (Oracle Always Free 여유 있음) | $0/월 | 콘솔 확인 필수. 여유 없으면 중단 |
| 테스트 Relay VM (무료 여유 없음, 소형 유료 VM) | 월 $3~6 (추정치) | 승인 없이는 생성하지 않음 |
| 도메인 (보유 도메인 서브도메인) | $0 추가 | 보유 여부 미확인 |
| 도메인 (무료 서브도메인, DuckDNS/deSEC) | $0 | 테스트용으로 충분 |
| 도메인 (신규 유료 도메인) | 연 $10~15 (추정치) | 필요 시에만 |
| TLS 인증서 (Let's Encrypt) | $0 (90일, 자동 갱신) | DNS-01 권장 |
| Mac·테스트 노드 | $0 | 기존 장비 |
| Oracle 운영 VM | $0 추가 | 변경 없음 |

최소 구성(무료 VM + 무료 서브도메인 + Let's Encrypt)이면 **추가 비용 $0**으로 테스트가
가능하다. 단, 무료 VM 여유와 보유 도메인 여부는 아직 미확인이다.

## 7. 위험 요소

1. **무료 자원 오판**: 콘솔 확인 없이 생성하면 유료 과금 또는 용량 부족 실패.
   생성 화면의 Always Free 표시를 확인하는 절차가 유일한 방지책이다.
2. **릴레이 오설정**: 빈 allowlist, 잘못 조립한 WSS 주소, 만료된 인증서.
   체크리스트(allowlist 비어 있지 않음·DNS·SAN·Peer ID 대조)로 방지한다.
3. **공공 와이파이 변수**: 캡티브 포털, 443 외 차단, DNS 변조, 세션 시간 제한.
   현장 실패를 릴레이 결함으로 단정하지 말고 환경 기록을 남긴다.
4. **개인키·인증서 유출**: 릴레이 신원키·TLS 키가 노출되면 Peer ID·도메인 신뢰가
   무너진다. 0600 유지, 백업은 암호화 보관, 저장소 업로드 금지.
5. **릴레이 남용**: allowlist가 뚫리거나 키가 유출되면 타인이 회선을 쓸 수 있다.
   회선당 16 MiB·32개 회선(구조적 상한 512 MiB) 한도가 있지만 집계 경보는 없다.
   테스트 기간에는 사용량을 사람이 확인한다.
6. **운영 침범**: 테스트 노드를 운영 서비스에 연결하면 크레딧·등록 상태가 오염된다.
   이번 테스트는 운영과 완전히 분리한다.
7. **단일 장애점**: 릴레이 1대뿐이라 릴레이 장애 시 양쪽 모두 연결이 끊긴다.
   재연결은 자동이지만, 테스트 중 릴레이 재시작은 계획된 1회로 제한한다.

## 8. 승인 전 결정 사항

- [ ] Oracle 콘솔 확인 결과: 무료 VM 여유 있음 / 없음 (사용자 확인)
- [ ] 도메인 결정: 보유 도메인 서브도메인 / 무료 서브도메인 신규 / 신규 유료 도메인
- [ ] 릴레이 VM 리전·shape (예: A1.Flex 1 OCPU/6 GB 또는 E2.1.Micro)
- [ ] 테스트 일정·장소(서울 공공 와이파이 위치)와 테스트 노드 실행 장소
- [ ] ResourceInfo `Decode` 크기 상한 추가 여부 (권고, 필수는 아님)
- [ ] 릴레이 systemd 서비스 등록 포함 여부
