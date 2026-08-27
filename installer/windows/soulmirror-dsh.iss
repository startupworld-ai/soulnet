; installer/windows/soulmirror-dsh.iss -- SoulMirror (DSH edition) Windows installer (Inno Setup 6).
;
; How to build: run scripts/build-installer-win.sh (it stages a portable
; Node.js, the dsh CLI, and the pre-installed web profile into
; %LOCALAPPDATA%\Temp\smdsh-stage, then compiles this script). Manual compile:
;   ISCC.exe /DAppVersion=0.1.1 /DFileVersion=0.1.1.0 ^
;            /DSourceDir=<abs stage dir> /DOutputDir=<abs dist dir> soulmirror-dsh.iss
;
; Shape: per-user install (no admin, no UAC) to %LOCALAPPDATA%\Programs\SoulMirror-DSH.
; NOTE the directory is deliberately NOT %LOCALAPPDATA%\Programs\SoulMirror --
; that is the main SoulMirror desktop product; this package must never
; overwrite it (separate AppId, separate directory). Unsigned, no autostart.
; Uninstall keeps the user's data (~\.dsh-soulmirror and ~\.soulnet).

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef FileVersion
  ; VersionInfoVersion needs plain a.b.c.d digits; the build script derives it.
  #define FileVersion "0.0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "..\..\.installer-stage\win" ; overridden by the build script (/DSourceDir)
#endif
#ifndef OutputDir
  #define OutputDir "..\..\dist"
#endif
#define AppName "SoulMirror"
#define AppSlug "SoulMirror-DSH"
#define AppPublisher "StartupWorld"
#define AppURL "https://github.com/startupworld-ai/soulnet"
#define LauncherCmd "bin\soulmirror-dsh.cmd"
#define DesktopExe "desktop\electron.exe"
#define DesktopApp "desktop\app"
#define IconFile "assets\soulmirror.ico"

[Setup]
; Fixed AppId: how Inno recognizes "the same product" for upgrades/uninstall.
; Distinct from the main SoulMirror desktop installer's AppId on purpose.
AppId={{6F1B7A9E-2D44-4C0B-8B7E-3A9D5C41F208}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion} (DSH edition)
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}
AppUpdatesURL={#AppURL}
VersionInfoVersion={#FileVersion}
VersionInfoProductTextVersion={#AppVersion}
VersionInfoDescription={#AppName} (DSH edition) installer
VersionInfoCompany={#AppPublisher}
DefaultDirName={localappdata}\Programs\{#AppSlug}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
; Per-user: no admin; installs under %LOCALAPPDATA%, registry under HKCU.
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir={#OutputDir}
OutputBaseFilename={#AppSlug}-Setup-{#AppVersion}
SetupIconFile={#IconFile}
UninstallDisplayIcon={app}\soulmirror.ico
UninstallDisplayName={#AppName} (DSH edition)
WizardStyle=modern
Compression=lzma2/max
SolidCompression=yes
LZMAUseSeparateProcess=yes
CloseApplications=yes
RestartApplications=no
ShowLanguageDialog=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Messages]
WelcomeLabel1=Welcome to the [name] Setup Wizard
WelcomeLabel2=This installs [name/ver]: DeepSeek Harness (dsh) with the SoulMirror network plugin, ready to run.%n%nEverything is installed into your user directory (no administrator rights needed). Your data lives in .dsh-soulmirror and .soulnet under your user profile and is kept on uninstall.
FinishedHeadingLabel=SoulMirror is installed
FinishedLabel=Click Finish to launch SoulMirror. A browser window opens on the local dsh web UI; the first run asks for a display name and creates your network identity.

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "{#SourceDir}\*"; DestDir: "{app}"; Flags: recursesubdirs createallsubdirs ignoreversion
Source: "{#IconFile}"; DestDir: "{app}"; DestName: "soulmirror.ico"

[Icons]
Name: "{autoprograms}\{#AppName}"; Filename: "{app}\{#DesktopExe}"; Parameters: """{app}\{#DesktopApp}"""; WorkingDir: "{app}"; IconFilename: "{app}\soulmirror.ico"; Comment: "SoulMirror on DeepSeek Harness"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#DesktopExe}"; Parameters: """{app}\{#DesktopApp}"""; WorkingDir: "{app}"; IconFilename: "{app}\soulmirror.ico"; Comment: "SoulMirror on DeepSeek Harness"; Tasks: desktopicon

[Run]
Filename: "{app}\{#DesktopExe}"; Parameters: """{app}\{#DesktopApp}"""; Description: "Launch {#AppName} now"; Flags: postinstall skipifsilent nowait

[UninstallDelete]
; Files the launcher or dsh may create inside the install dir (logs etc.).
Type: filesandordirs; Name: "{app}\app\node_modules\.cache"
