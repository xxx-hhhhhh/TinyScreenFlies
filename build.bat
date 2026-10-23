@echo off
setlocal
where go >nul 2>&1
if errorlevel 1 (
  echo Go not found. Install Go from https://go.dev/dl/
  pause
  exit /b 1
)
go build -ldflags="-H=windowsgui -s -w" -o "苍蝇爬屏幕.exe" .
if errorlevel 1 (
  echo Build failed.
  pause
  exit /b 1
)
echo Build complete: 苍蝇爬屏幕.exe
pause
