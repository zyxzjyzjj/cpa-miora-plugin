@echo off
REM Build the Miora CLIProxyAPI plugin on Windows.
REM
REM The plugin ABI is a C ABI, so CGO and a gcc-compatible toolchain are required
REM (MinGW-w64 gcc or LLVM-MinGW's x86_64-w64-mingw32-gcc). Set CC when gcc is
REM not on PATH, for example:
REM   set CC=C:\Program Files\LLVM-MinGW\bin\x86_64-w64-mingw32-gcc.exe
REM
REM Usage: build.bat
setlocal

cd /d "%~dp0"

set OUT_DIR=plugins\windows\amd64
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

echo building plugin -^> %OUT_DIR%\miora-provider.dll

set CGO_ENABLED=1
set GOOS=windows
set GOARCH=amd64

go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o "%OUT_DIR%\miora-provider.dll" .
if errorlevel 1 (
  echo.
  echo BUILD FAILED. If CGO could not find a compiler, install MinGW-w64 or
  echo LLVM-MinGW and point CC at the gcc-compatible driver.
  exit /b 1
)

echo.
echo done. Verify the ABI exports with:
echo   objdump -p %OUT_DIR%\miora-provider.dll ^| findstr cliproxy
