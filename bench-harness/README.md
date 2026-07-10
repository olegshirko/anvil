# Anvil bench harness

Сравнивает cold start / resume / compose-up время между vz-runner и
конкурентами на одинаковом workload.

## Setup

```bash
chmod +x run_bench.sh scripts/*.sh drivers/*.sh

# 1. Убедиться что vz-runner бинарник в PATH, либо:
export VZRUNNER_BIN=/path/to/.build/release/vz-runner

# 2. Затянуть образы заранее на каждом backend'е, которым будешь мерить
#    (иначе первый прогон меряет сеть, а не раннер)
./scripts/prepull.sh   # запускать после переключения docker context / open -a X

# 3. Убедиться что vz-runner расшаривает корень этой папки в VM на
#    /mnt/anvil через virtiofs (см. M2) — иначе vzc.sh не найдёт
#    workload-файл внутри VM.
```

## Запуск

```bash
./run_bench.sh vz-runner colima orbstack docker-desktop
# или
./run_bench.sh all
# или только свой раннер, если конкуренты не установлены
./run_bench.sh vz-runner
```

Каждый backend прогоняется изолированно: полный stop → cold start →
compose up (первый раз) → idle RSS → compose down → snapshot/stop →
resume → compose up (второй раз) → cleanup.

## Что меряется

| Фаза | Метрика | Что показывает |
|---|---|---|
| cold_start | daemon_ready | время от полного нуля до готовности принимать команды |
| cold_start | compose_up_healthy | + время поднять весь стек (db+cache+api+web) до healthy |
| resume | daemon_ready | то же, но после snapshot/resume (если backend поддерживает) вместо полного cold start |
| resume | compose_up_healthy | compose up на уже тёплом backend |
| steady_state | idle_rss_mb | память демона/VM-процесса на хосте в простое |

Для backend'ов без snapshot/resume API (Colima, OrbStack, Docker
Desktop на сегодня) "resume" фаза — это честный повторный cold start,
не подделка под resume. Это специально, чтобы разница была видна, а
не скрыта.

## Вывод

Результаты — `results/<timestamp>.csv` (сырые данные) и
`results/latest.md` (готовая таблица для README, с автоматическим
жирным выделением лучшего результата по каждой строке).

## Добавить новый backend

Скопируй `drivers/colima.sh` как шаблон, реализуй 7 функций
(`backend_name`, `backend_start`, `backend_stop`,
`backend_stop_keep_snapshot`, `backend_resume`,
`backend_compose_cmd`, `backend_all_healthy`, `backend_idle_rss`),
добавь имя в `ALL_BACKENDS` в `run_bench.sh`.
