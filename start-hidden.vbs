Option Explicit

' go-llm-proxy hidden launcher
' Used by the scheduled task at logon: starts the proxy with no console window.
Dim fso, shell, dir, exe, cfg
Set fso = CreateObject("Scripting.FileSystemObject")
Set shell = CreateObject("WScript.Shell")

dir = fso.GetParentFolderName(WScript.ScriptFullName)
exe = dir & "\go-llm-proxy.exe"
cfg = dir & "\config.yaml"

' Window style 0 = hidden; bWaitOnReturn = False (do not block)
shell.CurrentDirectory = dir
shell.Run """" & exe & """ -config """ & cfg & """", 0, False
