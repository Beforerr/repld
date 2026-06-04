repldDecode[hex_String] := FromCharacterCode[FromDigits[#, 16] & /@ StringPartition[hex, 2], "UTF-8"];
repldHex[text_] := StringJoin[IntegerString[#, 16, 2] & /@ ToCharacterCode[ToString[text], "UTF-8"]];

repldControl = Quiet @ Check[
  Module[{addr, token, parts, host, port, sock},
    addr = Environment["REPLD_CONTROL_ADDR"];
    token = Environment["REPLD_CONTROL_TOKEN"];
    If[!StringQ[addr] || !StringQ[token] || addr == "" || token == "", Return[None]];
    parts = StringSplit[addr, ":"];
    port = ToExpression[Last[parts]];
    host = StringRiffle[Most[parts], ":"];
    sock = SocketConnect[{host, port}];
    WriteString[sock, token <> "\n"];
    sock
  ],
  None
];

repldWriteControl[line_String] := If[Head[repldControl] === SocketObject, Quiet @ Check[WriteString[repldControl, line <> "\n"], Null]];

repldRun[hex_String, printResult_] := Module[{code, result, failed = False, short},
  code = repldDecode[hex];
  result = Check[ToExpression[code, InputForm], failed = True; $Failed];
  If[failed,
    short = "ERROR: evaluation failed";
    repldWriteControl["ERR " <> repldHex[short] <> " " <> repldHex[short <> "\n"] <> " " <> repldHex[short <> "\n"]];
    ,
    Print[result];
    repldWriteControl["OK"];
  ];
  Flush[$Output];
  Flush[$Messages];
];

While[True,
  line = InputString[];
  If[line === EndOfFile, Break[]];
  Which[
    StringStartsQ[line, "REPLD_EVAL "],
      parts = StringSplit[line, " "];
      repldRun[parts[[2]], ToExpression[parts[[3]]]],
    StringStartsQ[line, "REPLD_SENT "],
      sentinel = ToExpression[StringDrop[line, StringLength["REPLD_SENT "]]];
      WriteString[$Output, sentinel <> "\n"]; Flush[$Output];
      WriteString[OutputStream["stderr", 2], sentinel <> "\n"]; Flush[OutputStream["stderr", 2]];
  ];
];
