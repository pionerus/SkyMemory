@echo off
setlocal EnableDelayedExpansion

REM Run a `server.exe migrate <sub>` command with .env loaded.
REM Usage:
REM   migrate.bat               -> apply all pending migrations (default: 'up')
REM   migrate.bat up
REM   migrate.bat down
REM   migrate.bat version
REM   migrate.bat force 2

cd /d "%~dp0"

if not exist .env (
  echo [migrate] .env not found, copying from .env.example
  copy /y .env.example .env >nul
)

if not exist bin\server.exe (
  echo [migrate] bin\server.exe not found. Building...
  "C:\Program Files\Go\bin\go.exe" build -o bin\server.exe .\cmd\server || (
    echo ERROR: build failed
    exit /b 1
  )
)

for /f "usebackq eol=# tokens=1,* delims==" %%A in (".env") do (
  if not "%%A"=="" set "%%A=%%B"
)

set "SUB=%~1"
if "%SUB%"=="" set "SUB=up"

echo [migrate] running: server migrate %SUB% %2 %3
bin\server.exe migrate %SUB% %2 %3
