#ifndef PayloadDir
  #error PayloadDir is required
#endif
#ifndef OutputRoot
  #error OutputRoot is required
#endif
#ifndef BuildLabel
  #error BuildLabel is required
#endif

[Setup]
AppId={{7195A9EA-27C7-40C2-87BA-BF265B2536A4}
AppName=LoomGate
AppVersion={#BuildLabel}
AppPublisher=LoomGate
DefaultDirName=D:\LoomGate
DefaultGroupName=LoomGate
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
DisableProgramGroupPage=yes
DisableWelcomePage=no
WizardStyle=modern
OutputDir={#OutputRoot}
OutputBaseFilename=LoomGate-setup-amd64
Compression=lzma2
SolidCompression=yes
UninstallDisplayIcon={app}\program\LoomGate.exe
UninstallFilesDir={app}\program\uninstall
CloseApplications=yes
RestartApplications=no
SetupLogging=yes
LicenseFile={#PayloadDir}\LICENSE

[Tasks]
Name: desktopicon; Description: "Create a desktop shortcut"; GroupDescription: "Shortcuts:"; Flags: unchecked

[Files]
Source: "{#PayloadDir}\LoomGate.exe"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\wb2api.exe"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\login.exe"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\credit.exe"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\fission_bin.exe"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\release-signing.pub"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\manifest.json"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\README.txt"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\LICENSE"; DestDir: "{app}\program"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\bridge.py"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\managed.py"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\stream_bridge.py"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\tasks_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\stream_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\probe_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\probe_cleanup.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\requirements.txt"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion

[Icons]
Name: "{group}\LoomGate"; Filename: "{app}\program\LoomGate.exe"; WorkingDir: "{app}\program"
Name: "{autodesktop}\LoomGate"; Filename: "{app}\program\LoomGate.exe"; WorkingDir: "{app}\program"; Tasks: desktopicon

[Run]
Filename: "{app}\program\LoomGate.exe"; Description: "Launch LoomGate"; Flags: nowait postinstall skipifsilent

; No runtime data is installed or registered for removal. workspace/ is created
; by the launcher and deliberately survives repair, upgrade and uninstall.

[Code]
procedure InitializeWizard;
begin
  if not DirExists('D:\') then
    WizardForm.DirEdit.Text := ExpandConstant('{localappdata}\Programs\LoomGate');
  WizardForm.FinishedLabel.Caption := 'LoomGate is installed. Account and configuration data are stored in workspace next to program. Uninstall keeps workspace. This development build is not a production release.';
end;
