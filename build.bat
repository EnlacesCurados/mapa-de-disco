@echo off
rem Builds MapaDeDisco.exe. Needs Go 1.24 or newer: https://go.dev/dl/
go build -trimpath -ldflags "-s -w" -o MapaDeDisco.exe .
if errorlevel 1 (echo Build failed.) else (echo Done: MapaDeDisco.exe)
pause
