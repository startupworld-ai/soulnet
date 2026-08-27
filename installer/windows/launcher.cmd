@echo off
rem SoulMirror (DSH edition) launcher.
rem
rem Boots the DeepSeek Harness web profile with the portable Node.js runtime
rem bundled by the installer, with the soulnet-dsh plugin pre-installed.
rem Installed as <app>\bin\soulmirror-dsh.cmd; the Start menu / desktop
rem shortcuts point here. Extra arguments are forwarded to `dsh web`
rem (e.g. `soulmirror-dsh.cmd --no-open --port 3210`).
rem
rem Where things live:
rem   %DSH_HOME%            dsh home (profiles, sessions, settings).
rem                         Defaults to %USERPROFILE%\.dsh-soulmirror -- a
rem                         dedicated home, so an existing ~\.dsh of another
rem                         dsh setup is never touched.
rem   %USERPROFILE%\.soulnet  the soulnet identity / friends / conversations
rem                         (the plugin's default; override with SOULNET_HOME
rem                         or the `home` setting in Settings -> SoulMirror).
rem Neither directory is removed on uninstall.

setlocal EnableExtensions
set "ROOT=%~dp0.."
if not defined DSH_HOME set "DSH_HOME=%USERPROFILE%\.dsh-soulmirror"

rem Bundled node first; app\node_modules\.bin adds pnpm for `dsh plugin`.
set "PATH=%ROOT%\node;%ROOT%\app\node_modules\.bin;%PATH%"

set "TEMPLATE=%ROOT%\home-template"
set "PROFILE=%DSH_HOME%\profiles\web"
set "MARK=.soulmirror-template-version"

rem First run: seed the dsh home with the pre-installed web profile
rem (dsh-base + dsh-web-app + soulnet-dsh, node_modules included -- no
rem network, no pnpm needed).
if not exist "%PROFILE%\package.json" (
  echo First run: preparing the dsh profile at "%DSH_HOME%" ...
  robocopy "%TEMPLATE%" "%DSH_HOME%" /E /NFL /NDL /NJH /NJS >nul
)

rem Upgrade: when the installed template is newer than what this dsh home
rem was seeded with, refresh ONLY the packages we ship (never the user's
rem own plugins or settings).
fc /b "%TEMPLATE%\profiles\web\%MARK%" "%PROFILE%\%MARK%" >nul 2>&1
if errorlevel 1 (
  echo Updating the SoulMirror plugin in "%PROFILE%" ...
  for %%P in (soulnet-dsh soulnet-peer-windows-x64 soulnet-paygate-windows-x64 soulnet-dsh-sidebar) do (
    if exist "%TEMPLATE%\profiles\web\node_modules\%%P" (
      if exist "%PROFILE%\node_modules\%%P" rd /s /q "%PROFILE%\node_modules\%%P"
      robocopy "%TEMPLATE%\profiles\web\node_modules\%%P" "%PROFILE%\node_modules\%%P" /E /NFL /NDL /NJH /NJS >nul
    )
  )
  copy /y "%TEMPLATE%\profiles\web\%MARK%" "%PROFILE%\%MARK%" >nul
)

title SoulMirror
"%ROOT%\node\node.exe" "%ROOT%\app\node_modules\@deepseek-ai\dsh\lib\bin.js" web %*
endlocal
