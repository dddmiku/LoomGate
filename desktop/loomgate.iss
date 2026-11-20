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

[Languages]
Name: "chinesesimplified"; MessagesFile: "{#SourcePath}\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: desktopicon; Description: "创建桌面快捷方式"; GroupDescription: "快捷方式："; Flags: unchecked

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
Source: "{#PayloadDir}\doubao-runtime\work_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\stream_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\probe_runtime.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\probe_cleanup.js"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion
Source: "{#PayloadDir}\doubao-runtime\requirements.txt"; DestDir: "{app}\program\doubao-runtime"; Flags: ignoreversion

[Icons]
Name: "{group}\LoomGate"; Filename: "{app}\program\LoomGate.exe"; WorkingDir: "{app}\program"
Name: "{autodesktop}\LoomGate"; Filename: "{app}\program\LoomGate.exe"; WorkingDir: "{app}\program"; Tasks: desktopicon

[Run]
Filename: "{app}\program\LoomGate.exe"; Description: "启动 LoomGate"; Flags: nowait postinstall skipifsilent

; No runtime data is installed or registered for removal. workspace/ is created
; by the launcher and deliberately survives repair, upgrade and uninstall.

[Code]
procedure InitializeWizard;
begin
  if not DirExists('D:\') then
    WizardForm.DirEdit.Text := ExpandConstant('{localappdata}\Programs\LoomGate');
  WizardForm.FinishedLabel.Caption := 'LoomGate 已安装。账号与配置数据保存在 program 旁边的 workspace 目录，卸载会保留 workspace。';
end;
