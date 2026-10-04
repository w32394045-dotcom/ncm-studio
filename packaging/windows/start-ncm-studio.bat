# A one-click way to start the tool on Windows.
#
# The whole program is the .exe beside this file: it serves the UI on
# 127.0.0.1:8080 and needs no installation, no runtime and no account. This
# script exists only so that double-clicking starts it and opens the page, and
# leaves the window showing the log for anyone who wants to watch it work.
#
# Run `ncm-studio.bat -dir D:\music -workers 4` for anything more.

@echo off
setlocal
cd /d "%~dp0"

set "EXE="
for %%F in ("ncm-studio.exe") do if exist "%%~fF" set "EXE=%%~fF"
if not defined EXE (
  echo ncm-studio.exe was not found next to this file.
  echo Keep the .exe and this .bat together and try again.
  pause
  exit /b 1
)

rem The port can be overridden, and the browser is opened on whatever it is.
set "PORT=8080"
if not "%NCM_STUDIO_PORT%"=="" set "PORT=%NCM_STUDIO_PORT%"

echo Starting ncm-studio on http://127.0.0.1:%PORT%
echo Close this window (or press Ctrl-C) to stop it.
echo.

rem The page opens once the server is up rather than immediately, so the first
rem thing in the tab is the UI and not a connection error.
start "" /b cmd /c "timeout /t 2 /nobreak >nul & start "" http://127.0.0.1:%PORT%/"

"%EXE%" -port %PORT% %*
set "CODE=%ERRORLEVEL%"

rem A non-zero exit means the server could not start — almost always the port
rem already being in use by another copy of this program. Saying so beats a
rem window that closes before anyone can read it.
if not "%CODE%"=="0" (
  echo.
  echo ncm-studio exited with code %CODE%.
  echo If it says the port is in use, start it on another one:
  echo    ncm-studio.bat -port 8081
  pause
)
endlocal
