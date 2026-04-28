@echo off
setlocal EnableDelayedExpansion

REM Load .env (skip lines starting with #) and run cloud server.
REM Usage: double-click, or run from any cmd.exe / PowerShell.

cd /d "%~dp0"

if not exist .env (
  echo [run-server] .env not found, copying from .env.example
  if not exist .env.example (
    echo ERROR: .env.example also missing — repo is incomplete.
    exit /b 1
  )
  copy /y .env.example .env >nul
)

if not exist bin\server.exe (
  echo [run-server] bin\server.exe not found. Building...
  "C:\Program Files\Go\bin\go.exe" build -o bin\server.exe .\cmd\server || (
    echo ERROR: build failed
    exit /b 1
  )
)

REM Parse .env. eol=# skips comment lines, tokens=1,* delims== splits on first '='.
for /f "usebackq eol=# tokens=1,* delims==" %%A in (".env") do (
  if not "%%A"=="" set "%%A=%%B"
)

echo [run-server] starting on %FREEFALL_HTTP_ADDR%
bin\server.exe
