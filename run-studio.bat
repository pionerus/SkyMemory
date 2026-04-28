@echo off
setlocal EnableDelayedExpansion

REM Load .env (skip lines starting with #) and run the studio.
REM Studio doesn't strictly need a database — runs standalone.

cd /d "%~dp0"

if not exist .env (
  echo [run-studio] .env not found, copying from .env.example
  if not exist .env.example (
    echo ERROR: .env.example also missing — repo is incomplete.
    exit /b 1
  )
  copy /y .env.example .env >nul
)

if not exist bin\studio.exe (
  echo [run-studio] bin\studio.exe not found. Building...
  "C:\Program Files\Go\bin\go.exe" build -o bin\studio.exe .\cmd\studio || (
    echo ERROR: build failed
    exit /b 1
  )
)

for /f "usebackq eol=# tokens=1,* delims==" %%A in (".env") do (
  if not "%%A"=="" set "%%A=%%B"
)

echo [run-studio] starting on http://%STUDIO_HTTP_ADDR%
echo [run-studio] cloud target: %STUDIO_CLOUD_BASE_URL%
bin\studio.exe
