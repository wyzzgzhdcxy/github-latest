@echo off
REM ============================================================================
REM build.bat - build gh-latest as a Windows GUI subsystem binary
REM
REM -H windowsgui sets the PE subsystem to IMAGE_SUBSYSTEM_WINDOWS_GUI (2).
REM Effect: Windows does NOT allocate a console when launching the exe,
REM          so no black window pops up under Task Scheduler.
REM
REM Caveat: stdout/stderr are NOT connected to any console in GUI mode.
REM          Anything you print to them is silently discarded.
REM          For Task Scheduler usage, log to a file instead (e.g. --output,
REM          or wrap with `>> log.txt 2>&1` after switching back to console).
REM ============================================================================

setlocal
set "BIN_NAME=gh-latest.exe"
set "LDFLAGS=-s -w -H windowsgui"

echo === Building %BIN_NAME% (Windows GUI subsystem) ===

go mod download
if errorlevel 1 (
    echo [FAIL] go mod download
    exit /b 1
)

go build -trimpath -ldflags "%LDFLAGS%" -o "%BIN_NAME%" .
if errorlevel 1 (
    echo [FAIL] go build
    exit /b 1
)

echo [OK] %BIN_NAME% built at %~dp0%BIN_NAME%
echo.
echo ---- Task Scheduler settings ----
echo Program : %~dp0%BIN_NAME%
echo Arguments: --repo owner/name --platforms windows --archs amd64
echo.
echo ---- Useful flags for this project ----
echo --from-file list.txt   batch mode: read URLs from list
echo --output result.txt    batch mode: write URLs to file
echo --download-dir D:\x    where downloaded files end up
echo --proxy ""             disable the default 127.0.0.1:10808 proxy
echo.
echo ---- Debug builds (keep console) ----
echo   go build -o gh-latest-debug.exe .
echo   (run gh-latest-debug.exe from a terminal to see logs)

endlocal
exit /b 0
