repldDecode[hex_String] := FromCharacterCode[FromDigits[#, 16] & /@ StringPartition[hex, 2], "UTF-8"];
repldHex[text_] := StringJoin[IntegerString[#, 16, 2] & /@ ToCharacterCode[ToString[text], "UTF-8"]];

(* -script mode points $Messages at stdout; redirect so message text reaches
   the user via stderr *)
$Messages = {OutputStream["stderr", 2]};

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

(* Messages don't imply failure in Wolfram (1/0 emits Power::infy yet returns
   ComplexInfinity), so let them stream to stderr and only signal ERR on a
   genuine failure *)
repldRun[hex_String, printResult_] := Block[{$MessageList = {}},
  Module[{code, raw, result, aborted = False, thrown = False, failed, msgs, short},
    code = repldDecode[hex];
    raw = CheckAbort[
      Catch[
        Catch[repld`Done[Quiet[ToExpression[code, InputForm], {}]]],
        _,
        (thrown = True; repld`Thrown[#1]) &
      ],
      aborted = True; repld`Done[$Failed]
    ];
    Which[
      MatchQ[raw, repld`Done[_]], result = First[raw],
      MatchQ[raw, repld`Thrown[_]], thrown = True; result = First[raw],
      True, thrown = True; result = raw
    ];
    msgs = ToString /@ $MessageList;
    failed = aborted || thrown || (result === $Failed);
    If[failed,
      short = "ERROR: " <> If[msgs === {}, "evaluation failed", StringRiffle[msgs, ", "]];
      repldWriteControl["ERR " <> repldHex[short] <> " " <> repldHex[short <> "\n"] <> " " <> repldHex[short <> "\n"]];
      ,
      (* Match native `wolframscript -code`: print the result unconditionally,
         Null included, in ToString/OutputForm (strings render unquoted). *)
      If[printResult, WriteString[$Output, ToString[result] <> "\n"]];
      repldWriteControl["OK"];
    ];
    Flush[$Output];
    Flush[OutputStream["stderr", 2]];
  ]
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
