@echo off
chcp 65001 > nul
cd /d "%~dp0"
echo [1/2] Embedding Windows application icon...
where rsrc >nul 2>nul
if %ERRORLEVEL% equ 0 (
    rsrc -ico icon.ico -o rsrc.syso
) else if exist "C:\Users\Lenovo\go\bin\rsrc.exe" (
    "C:\Users\Lenovo\go\bin\rsrc.exe" -ico icon.ico -o rsrc.syso
)

echo [2/2] Compiling ImageTileCutter with mygo build (including auto-update signature)...
if not defined MYGO_UPDATER_PRIVATE_KEY (
    if exist "%APPDATA%\mygo\update-keys\mygo-update.key" (
        set /p MYGO_UPDATER_PRIVATE_KEY=<"%APPDATA%\mygo\update-keys\mygo-update.key"
    )
)

where mygo >nul 2>nul
if %ERRORLEVEL% equ 0 (
    mygo build -skip-installer
) else if exist "C:\Users\Lenovo\go\bin\mygo.exe" (
    "C:\Users\Lenovo\go\bin\mygo.exe" build -skip-installer
) else (
    go build -ldflags "-s -w -H windowsgui" -o "ImageTileCutter.exe" .
)
set BUILD_CODE=%ERRORLEVEL%

if exist "build\windows-amd64\ImageTileCutter.exe" (
    copy /y "build\windows-amd64\ImageTileCutter.exe" "ImageTileCutter.exe" >nul 2>nul
)

if %BUILD_CODE% equ 0 (
    echo.
    echo ====================================================
    echo [BUILD SUCCESS] ImageTileCutter.exe is ready with embedded icon and update signatures.
    echo ====================================================
) else (
    echo [BUILD FAILED] Code: %BUILD_CODE%
)
